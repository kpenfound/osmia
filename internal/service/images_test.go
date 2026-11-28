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
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func attachmentPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	fixturekit.Must(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	return b.Bytes()
}

type imageRig struct {
	s      *Service
	repo   *trace.Repository
	data   []byte
	image  trace.Image
	events []api.Event
}

func newImageRig(t *testing.T) *imageRig {
	t.Helper()
	opts := eventTraceOptions(t)
	fixturekit.WithListen(t, opts.Config.Root, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	withTailnet(t, &opts)
	s, _ := start(t, opts)
	repo := s.sole().repository
	data := attachmentPNG(t)
	sub, ok := s.hub.Subscribe()
	if !ok {
		t.Fatal("subscribe")
	}
	t.Cleanup(func() { s.hub.Unsubscribe(sub) })
	sub.Take()
	h := trace.Header{Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "image"}
	v, err := repo.RecordImage(context.Background(), h, "preview-key", "image/png", data, "turn")
	fixturekit.Must(t, err)
	return &imageRig{s: s, repo: repo, data: data, image: v, events: sub.Take()}
}

func TestRecordingTheSameImageTwiceKeepsItsID(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	h := trace.Header{Project: r.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "image"}
	again, err := r.repo.RecordImage(context.Background(), h, "preview-key", "image/png", r.data, "turn")
	fixturekit.Must(t, err)
	if again.ID != r.image.ID {
		t.Fatal("retry changed ID")
	}
}

func TestRecordingAnImageAnnouncesAnImageEvent(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	if !slices.Contains(r.events, api.Event{Kind: api.EventImage, Project: r.repo.Project(), Workstream: fixturekit.Stream}) {
		t.Fatalf("no image event: %+v", r.events)
	}
}

func TestTheFeedShowsAnImageOnceWithoutReorderingEqualTimestampEntries(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	feed, apiErr := r.s.feedList(string(fixturekit.Stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	n := 0
	seenImage := false
	for _, entry := range feed.Entries {
		if entry.At.Equal(fixturekit.DemoStart) {
			if seenImage && entry.Kind != "image" {
				t.Fatal("image reordered existing equal-timestamp entries")
			}
			seenImage = seenImage || entry.Kind == "image"
		}
		if entry.Kind == "image" {
			n++
			if entry.Image == nil || entry.Image.URL != imageURL(fixturekit.Stream, r.image.ID) {
				t.Fatalf("image: %+v", entry)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d feed images", n)
	}
}

func TestTheImageEndpointServesTheImageOnlyToHostsOfTheListener(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	s := r.s
	for _, c := range []struct {
		base, host string
		client     *http.Client
		allow      bool
	}{
		{"http://osmia", "osmia", socketHTTP(s.Socket()), true},
		{"http://" + s.WebAddr(), "localhost", intakefixture.WebHTTP(), true},
		{"http://" + s.WebAddr(), "evil.invalid", intakefixture.WebHTTP(), false},
		{"http://" + s.TailnetAddr(), "osmia", intakefixture.WebHTTP(), true},
		{"http://" + s.TailnetAddr(), "evil.invalid", intakefixture.WebHTTP(), false},
	} {
		req, err := http.NewRequest(http.MethodGet, c.base+imageURL(fixturekit.Stream, r.image.ID), nil)
		fixturekit.Must(t, err)
		req.Host = c.host
		resp, err := c.client.Do(req)
		fixturekit.Must(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		fixturekit.Must(t, err)
		if c.allow {
			if resp.StatusCode != 200 || !bytes.Equal(body, r.data) || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("image response: %d %v", resp.StatusCode, resp.Header)
			}
		} else if resp.StatusCode != 403 {
			t.Fatalf("host: %d", resp.StatusCode)
		}
	}
}

func TestTheImageEndpointAnswersNotFoundForAnUnknownImage(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	rec := httptest.NewRecorder()
	r.s.handle(rec, httptest.NewRequest(http.MethodGet, imageURL(fixturekit.Stream, "unknown"), nil))
	if rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
}

func TestImageBytesNeverAppearInTheFeedEventsOrConfiguration(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	feed, apiErr := r.s.feedList(string(fixturekit.Stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	for _, value := range []any{feed, r.events, r.s.configuration()} {
		payload, err := json.Marshal(value)
		fixturekit.Must(t, err)
		if bytes.Contains(payload, r.data) || strings.Contains(string(payload), base64.StdEncoding.EncodeToString(r.data)) {
			t.Fatal("bytes in metadata")
		}
	}
}

type groupImage struct {
	s       *Service
	manager *trace.Repository
	g       trace.Workgroup
	repo    *trace.Repository
	member  config.WorkstreamID
	host    config.WorkstreamID
	header  trace.Header
	image   trace.Image
	data    []byte
}

func eachGroupKind(t *testing.T, check func(t *testing.T, cross bool)) {
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "cross"}[cross], func(t *testing.T) { check(t, cross) })
	}
}

func newGroupImage(t *testing.T, cross bool) *groupImage {
	t.Helper()
	s, manager, g := workgroupFeedFixture(t, cross, 2)
	ctx := context.Background()
	member := fixturekit.Quiet
	repo := s.projects[len(s.projects)-1].repository
	data := attachmentPNG(t)
	h := trace.Header{Project: repo.Project(), Workstream: member, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "preview"}
	v, err := repo.RecordImage(ctx, h, "preview", "image/png", data, "turn")
	fixturekit.Must(t, err)
	host, err := manager.ManagerHost(g.Members[0])
	fixturekit.Must(t, err)
	h.Project, h.Workstream = manager.Project(), host
	for range 2 {
		_, err = manager.PublishImageReference(ctx, h, member, v.ID, "turn")
		fixturekit.Must(t, err)
	}
	return &groupImage{s: s, manager: manager, g: g, repo: repo, member: member, host: host, header: h, image: v, data: data}
}

func (i *groupImage) feedImages(t *testing.T) []FeedEntry {
	t.Helper()
	feed, apiErr := i.s.workgroupFeed(context.Background(), string(i.g.ID))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	var out []FeedEntry
	for _, e := range feed.Entries {
		if e.Kind == "image" {
			out = append(out, e)
		}
	}
	return out
}

func (i *groupImage) archiveMember(t *testing.T, cross bool) runtime.Archive {
	t.Helper()
	ctx := context.Background()
	if cross {
		pins := []trace.GroupMemberApproval{{Project: i.s.projects[0].id, Workstream: fixturekit.Stream}, {Project: i.repo.Project(), Workstream: i.member}}
		fixturekit.Must(t, trace.RecordGroupApproval(ctx, i.manager, i.g.Members[0], trace.GroupApproval{Workgroup: i.g.ID, Members: pins, Complete: true}, fixturekit.DemoStart, ownerActor))
		fixturekit.Must(t, i.manager.PreserveMemberQuestions(ctx, i.g, trace.CrossRepoMember{Workstream: i.member, Project: i.repo}, fixturekit.DemoStart, serviceActor))
	}
	a := runtime.Archive{Project: i.repo.Project(), Workstream: i.member, Workgroup: i.g.ID, State: AbandonedState, Title: "member", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: fixturekit.DemoStart, CleanupStartedAt: fixturekit.DemoStart, CleanedAt: fixturekit.DemoStart}
	fixturekit.Must(t, i.s.store.SetArchived(a))
	_, err := i.repo.MarkWorkstreamTraceForDeletion(i.member)
	fixturekit.Must(t, err)
	fixturekit.Must(t, trace.FinishWorkstreamTraceDeletion(i.s.cfg.Root, i.repo.Project(), i.member))
	return a
}

func TestWorkgroupFeedShowsAMembersImageOnceWithItsAttributionAndURL(t *testing.T) {
	eachGroupKind(t, func(t *testing.T, cross bool) {
		i := newGroupImage(t, cross)
		entries := i.feedImages(t)
		if len(entries) != 1 {
			t.Fatalf("%d reference entries", len(entries))
		}
		e := entries[0]
		if e.Workstream != i.member || e.Repo != i.repo.Project() || e.Image.ID != i.image.ID || e.Image.URL != imageURL(i.member, i.image.ID) {
			t.Fatalf("image entry: %+v", e)
		}
	})
}

func TestPublishingAnImageReferenceTwiceKeepsOneReference(t *testing.T) {
	eachGroupKind(t, func(t *testing.T, cross bool) {
		i := newGroupImage(t, cross)
		refs, err := trace.Read[trace.ImageReference](i.manager, i.host)
		fixturekit.Must(t, err)
		if len(refs) != 1 {
			t.Fatalf("refs %d", len(refs))
		}
	})
}

func TestACrossProjectWorkgroupHoldsNoImageBytes(t *testing.T) {
	i := newGroupImage(t, true)
	images, err := trace.Read[trace.Image](i.manager, i.host)
	fixturekit.Must(t, err)
	if len(images) != 0 {
		t.Fatal("bytes in group")
	}
	if _, err = i.manager.RecordImage(context.Background(), i.header, "forbidden", "image/png", i.data, "turn"); err == nil {
		t.Fatal("group accepted bytes")
	}
}

func TestWorkgroupFeedShowsAPlaceholderForAnArchivedMembersImage(t *testing.T) {
	eachGroupKind(t, func(t *testing.T, cross bool) {
		i := newGroupImage(t, cross)
		i.archiveMember(t, cross)
		entries := i.feedImages(t)
		if len(entries) != 1 {
			t.Fatalf("%d reference entries", len(entries))
		}
		if e := entries[0]; e.Workstream != i.member || e.Repo != i.repo.Project() || e.Image.ID != i.image.ID || e.Image.URL != "" || e.Image.Placeholder != "image removed when "+string(i.member)+" was archived" {
			t.Fatalf("placeholder: %+v", e)
		}
	})
}

func TestAnArchivedMembersImageEndpointIsGone(t *testing.T) {
	eachGroupKind(t, func(t *testing.T, cross bool) {
		i := newGroupImage(t, cross)
		i.archiveMember(t, cross)
		rec := httptest.NewRecorder()
		i.s.handle(rec, httptest.NewRequest(http.MethodGet, imageURL(i.member, i.image.ID), nil))
		if rec.Code != 404 {
			t.Fatalf("archived endpoint: %d", rec.Code)
		}
	})
}

func TestTheArchiveRecordOfAMemberPinsNoImageBytes(t *testing.T) {
	eachGroupKind(t, func(t *testing.T, cross bool) {
		i := newGroupImage(t, cross)
		payload, err := json.Marshal(i.archiveMember(t, cross))
		fixturekit.Must(t, err)
		if strings.Contains(string(payload), base64.StdEncoding.EncodeToString(i.data)) {
			t.Fatal("archive pinned bytes")
		}
	})
}

func TestImagesDoNotEnterMemoryKnowledgeOrNotifications(t *testing.T) {
	hook := fixturekit.NewWebhook(t)
	opts, _ := conversationFixture(t, "image-privacy-")
	inbox := &fixturekit.NotificationInbox{}
	opts.notifyInbox = inbox.Read
	fixturekit.SetWebhook(t, opts.Config.Root, hook.Hook())
	s, _ := startNotifying(t, opts, inbox)
	repo := s.sole().repository
	data := attachmentPNG(t)
	_, err := repo.RecordImage(context.Background(), trace.Header{Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "preview"}, "preview", "image/png", data, "turn")
	fixturekit.Must(t, err)
	noBytes := func(label string, value any) {
		t.Helper()
		payload, err := json.Marshal(value)
		fixturekit.Must(t, err)
		if bytes.Contains(payload, data) || strings.Contains(string(payload), base64.StdEncoding.EncodeToString(data)) {
			t.Fatalf("%s contains image bytes", label)
		}
	}
	local := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }}
	knowledge, err := local.Assemble(context.Background(), repo.Project(), bundle.Scope{Role: sessionRole, Workstream: fixturekit.Stream})
	fixturekit.Must(t, err)
	noBytes("file context", knowledge)
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
	cfg.Hearsay = config.Hearsay{URL: server.URL, Principal: "owner", TokenEnv: "OWNER", Agents: map[string]config.HearsayAgent{"observer": {ID: "observer", TokenEnv: "AGENT"}}}
	cfg.Projects = slices.Clone(cfg.Projects)
	cfg.Projects[0].HearsayScope = "images-test"
	cfg.Project.HearsayScope = "images-test"
	provider := hearsay.Provider{Local: local, Config: func() *config.Config { return &cfg }, Client: func(h config.Hearsay) hearsay.Client {
		return hearsay.Client{Config: h, LookupEnv: func(string) string { return "test-token" }}
	}}
	_, err = provider.Assemble(context.Background(), repo.Project(), bundle.Scope{Role: sessionRole, Workstream: fixturekit.Stream})
	fixturekit.Must(t, err)
	if len(requests) == 0 {
		t.Fatal("Hearsay was not exercised")
	}
	for _, raw := range requests {
		noBytes("Hearsay request", string(raw))
	}
	s.notifier.pass(context.Background())
	inbox.Set(fixturekit.NotificationEntry(InboxEscalation, 1))
	s.notifier.pass(context.Background())
	posts := intakefixture.AwaitWebhook(t, hook, 1)
	for _, post := range posts {
		noBytes("webhook", post)
	}
}
