package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/trace"
)

func attachmentPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	must(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	return b.Bytes()
}

func TestImageEndpointAndFeed(t *testing.T) {
	t.Parallel()
	opts := eventTraceOptions(t)
	withListen(t, opts, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	withTailnet(t, &opts)
	s, _ := start(t, opts)
	repo := s.sole().repository
	data := attachmentPNG(t)
	sub, ok := s.hub.Subscribe()
	if !ok {
		t.Fatal("subscribe")
	}
	defer s.hub.Unsubscribe(sub)
	sub.Take()
	h := trace.Header{Project: repo.Project(), Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "image"}
	v, err := repo.RecordImage(context.Background(), h, "preview-key", "image/png", data, "turn")
	must(t, err)
	again, err := repo.RecordImage(context.Background(), h, "preview-key", "image/png", data, "turn")
	must(t, err)
	if again.ID != v.ID {
		t.Fatal("retry changed ID")
	}
	events := sub.Take()
	if !slices.Contains(events, api.Event{Kind: api.EventImage, Project: repo.Project(), Workstream: stream}) {
		t.Fatalf("no image event: %+v", events)
	}
	feed, apiErr := s.feedList(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	n := 0
	seenImage := false
	for _, entry := range feed.Entries {
		if entry.At.Equal(demoStart) {
			if seenImage && entry.Kind != "image" {
				t.Fatal("image reordered existing equal-timestamp entries")
			}
			seenImage = seenImage || entry.Kind == "image"
		}
		if entry.Kind == "image" {
			n++
			if entry.Image == nil || entry.Image.URL != imageURL(stream, v.ID) {
				t.Fatalf("image: %+v", entry)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d feed images", n)
	}
	for _, c := range []struct {
		base, host string
		client     *http.Client
		allow      bool
	}{
		{"http://osmia", "osmia", socketHTTP(s.Socket()), true},
		{"http://" + s.WebAddr(), "localhost", webHTTP(), true},
		{"http://" + s.WebAddr(), "evil.invalid", webHTTP(), false},
		{"http://" + s.TailnetAddr(), "osmia", webHTTP(), true},
		{"http://" + s.TailnetAddr(), "evil.invalid", webHTTP(), false},
	} {
		req, err := http.NewRequest(http.MethodGet, c.base+imageURL(stream, v.ID), nil)
		must(t, err)
		req.Host = c.host
		resp, err := c.client.Do(req)
		must(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		must(t, err)
		if c.allow {
			if resp.StatusCode != 200 || !bytes.Equal(body, data) || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("image response: %d %v", resp.StatusCode, resp.Header)
			}
		} else if resp.StatusCode != 403 {
			t.Fatalf("host: %d", resp.StatusCode)
		}
	}

	rec := httptest.NewRecorder()
	s.handle(rec, httptest.NewRequest(http.MethodGet, imageURL(stream, "unknown"), nil))
	if rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
	// Text exports and API metadata never contain encoded attachment bytes.
	for _, value := range []any{feed, events, s.configuration()} {
		payload, err := json.Marshal(value)
		must(t, err)
		if bytes.Contains(payload, data) || strings.Contains(string(payload), base64.StdEncoding.EncodeToString(data)) {
			t.Fatal("bytes in metadata")
		}
	}
}

func TestWorkgroupImageReferenceArchive(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "cross"}[cross], func(t *testing.T) {
			s, chief, g := workgroupFeedFixture(t, cross, 2)
			ctx := context.Background()
			member := fixturekit.Quiet
			repo := s.projects[len(s.projects)-1].repository
			data := attachmentPNG(t)
			h := trace.Header{Project: repo.Project(), Workstream: member, At: demoStart, Actor: serviceActor, Cause: "preview"}
			v, err := repo.RecordImage(ctx, h, "preview", "image/png", data, "turn")
			must(t, err)
			host, err := chief.ChiefOfStaffHost(g.Members[0])
			must(t, err)
			h.Project, h.Workstream = chief.Project(), host
			for range 2 {
				_, err = chief.PublishImageReference(ctx, h, member, v.ID, "turn")
				must(t, err)
			}
			check := func(removed bool) {
				t.Helper()
				feed, apiErr := s.workgroupFeed(ctx, string(g.ID))
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				n := 0
				for _, e := range feed.Entries {
					if e.Kind != "image" {
						continue
					}
					n++
					if e.Workstream != member || e.Repo != repo.Project() || e.Image.ID != v.ID {
						t.Fatalf("attribution: %+v", e)
					}
					if removed {
						if e.Image.URL != "" || e.Image.Placeholder != "image removed when "+string(member)+" was archived" {
							t.Fatalf("placeholder: %+v", e.Image)
						}
					} else if e.Image.URL != imageURL(member, v.ID) {
						t.Fatalf("URL: %+v", e.Image)
					}
				}
				if n != 1 {
					t.Fatalf("%d reference entries", n)
				}
			}
			check(false)
			refs, err := trace.Read[trace.ImageReference](chief, host)
			must(t, err)
			if len(refs) != 1 {
				t.Fatalf("refs %d", len(refs))
			}
			if cross {
				images, err := trace.Read[trace.Image](chief, host)
				must(t, err)
				if len(images) != 0 {
					t.Fatal("bytes in group")
				}
				_, err = chief.RecordImage(ctx, h, "forbidden", "image/png", data, "turn")
				if err == nil {
					t.Fatal("group accepted bytes")
				}
				pins := []trace.GroupMemberApproval{{Project: s.projects[0].id, Workstream: stream}, {Project: repo.Project(), Workstream: member}}
				must(t, trace.RecordGroupApproval(ctx, chief, g.Members[0], trace.GroupApproval{Workgroup: g.ID, Members: pins, Complete: true}, demoStart, ownerActor))
				must(t, chief.PreserveMemberQuestions(ctx, g, trace.CrossRepoMember{Workstream: member, Project: repo}, demoStart, serviceActor))
			}
			a := runtime.Archive{Project: repo.Project(), Workstream: member, Workgroup: g.ID, State: AbandonedState, Title: "member", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: demoStart, CleanupStartedAt: demoStart, CleanedAt: demoStart}
			must(t, s.store.SetArchived(a))
			_, err = repo.MarkWorkstreamTraceForDeletion(member)
			must(t, err)
			must(t, trace.FinishWorkstreamTraceDeletion(s.cfg.Root, repo.Project(), member))
			check(true)
			rec := httptest.NewRecorder()
			s.handle(rec, httptest.NewRequest(http.MethodGet, imageURL(member, v.ID), nil))
			if rec.Code != 404 {
				t.Fatalf("archived endpoint: %d", rec.Code)
			}
			payload, err := json.Marshal(a)
			must(t, err)
			if strings.Contains(string(payload), base64.StdEncoding.EncodeToString(data)) {
				t.Fatal("archive pinned bytes")
			}
		})
	}
}

func TestImagesDoNotEnterMemoryKnowledgeOrNotifications(t *testing.T) {
	hook := newFakeWebhook(t)
	opts, _ := conversationFixture(t, "image-privacy-")
	inbox := &fakeInbox{}
	opts.notifyInbox = inbox.read
	setWebhook(t, opts, hook.hook())
	s, _ := startNotifying(t, opts, inbox)
	repo := s.sole().repository
	data := attachmentPNG(t)
	_, err := repo.RecordImage(context.Background(), trace.Header{Project: repo.Project(), Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "preview"}, "preview", "image/png", data, "turn")
	must(t, err)
	noBytes := func(label string, value any) {
		t.Helper()
		payload, err := json.Marshal(value)
		must(t, err)
		if bytes.Contains(payload, data) || strings.Contains(string(payload), base64.StdEncoding.EncodeToString(data)) {
			t.Fatalf("%s contains image bytes", label)
		}
	}
	// A real file-context assembly supplies the librarian's knowledge context.
	local := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }}
	knowledge, err := local.Assemble(context.Background(), repo.Project(), bundle.Scope{Role: "librarian", Workstream: stream})
	must(t, err)
	noBytes("knowledge context", knowledge)
	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests = append(requests, raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"scope":{"id":"images-test"}}`)
	}))
	defer server.Close()
	cfg := *s.current()
	cfg.Hearsay = config.Hearsay{URL: server.URL, Principal: "owner", TokenEnv: "OWNER", Agents: map[string]config.HearsayAgent{"observer": {ID: "librarian", TokenEnv: "AGENT"}}}
	cfg.Projects = slices.Clone(cfg.Projects)
	cfg.Projects[0].HearsayScope = "images-test"
	cfg.Project.HearsayScope = "images-test"
	provider := hearsay.Provider{Local: local, Config: func() *config.Config { return &cfg }, Client: func(h config.Hearsay) hearsay.Client {
		return hearsay.Client{Config: h, LookupEnv: func(string) string { return "test-token" }}
	}}
	_, err = provider.Assemble(context.Background(), repo.Project(), bundle.Scope{Role: "librarian", Workstream: stream})
	must(t, err)
	if len(requests) == 0 {
		t.Fatal("Hearsay was not exercised")
	}
	for _, raw := range requests {
		noBytes("Hearsay request", string(raw))
	}
	s.notifier.pass(context.Background())
	inbox.set(entryOf(InboxEscalation, 1))
	s.notifier.pass(context.Background())
	posts := hook.await(t, 1)
	for _, post := range posts {
		noBytes("webhook", post)
	}
}
