package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// hooksPost is one request the fake hooks endpoint received.
type hooksPost struct {
	Source string
	Auth   string
	Raw    string
	Body   map[string]any
}

func (p hooksPost) str(field string) string { s, _ := p.Body[field].(string); return s }

// hooksFake stands in for the connectors listener of Hearsay's work source.
// It checks nothing of Hearsay's own validation; it records the requests the
// publisher makes, and answers with the status it is set to.
type hooksFake struct {
	*httptest.Server
	mu     sync.Mutex
	posts  []hooksPost
	status int
}

func newHooksFake(t *testing.T) *hooksFake { return newHooksFakeOn(t, "127.0.0.1:0") }

// newHooksFakeOn starts a hooks fake listening on addr, so a fake that was
// closed can come back at the address the publisher is configured with.
func newHooksFakeOn(t *testing.T, addr string) *hooksFake {
	t.Helper()
	f := &hooksFake{status: http.StatusAccepted}
	listener, err := net.Listen("tcp", addr)
	must(t, err)
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		post := hooksPost{Source: strings.TrimPrefix(r.URL.Path, "/hooks/"), Auth: r.Header.Get("Authorization"), Raw: string(raw)}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/hooks/") || json.Unmarshal(raw, &post.Body) != nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		f.mu.Lock()
		status := f.status
		if status == http.StatusAccepted {
			f.posts = append(f.posts, post)
		}
		f.mu.Unlock()
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			_, _ = w.Write([]byte(`{"id":"evt:fake"}`))
		}
	}))
	f.Server.Listener.Close()
	f.Server.Listener = listener
	f.Server.Start()
	t.Cleanup(f.Close)
	return f
}

func (f *hooksFake) setStatus(status int) { f.mu.Lock(); f.status = status; f.mu.Unlock() }

// take returns the requests received since the last take.
func (f *hooksFake) take() []hooksPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.posts
	f.posts = nil
	return out
}

const publishTestToken = "publisher-token-8a7b6c5d"

// publisherFixture is a project with two workstreams and a charter, a trace
// the test holds open, and a service configured to publish to hooks.
type publisherFixture struct {
	t     *testing.T
	s     *Service
	cfg   *config.Config
	repo  *trace.Repository
	hooks *hooksFake
}

func newPublisherFixture(t *testing.T, mutate ...func(*config.Config)) *publisherFixture {
	t.Helper()
	// serial: sets the publisher's token in the process environment.
	t.Setenv("PUBLISH_TEST_TOKEN", publishTestToken)
	hooks := newHooksFake(t)
	_, cfg := conversationFixture(t, "hearsay-publish-")
	cfg.Hearsay = config.Hearsay{URL: "https://memory.example", Principal: "owner", TokenEnv: "PUBLISH_TEST_OWNER", Agents: map[string]config.HearsayAgent{
		"worker": {ID: "osmia-worker", TokenEnv: "PUBLISH_TEST_WORKER"}, "orchestrator": {ID: "osmia-chief", TokenEnv: "PUBLISH_TEST_CHIEF"}, "observer": {ID: "osmia-observer", TokenEnv: "PUBLISH_TEST_OBSERVER"}},
		Publish: &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: hooks.URL, Agent: "osmia-publisher", TokenEnv: "PUBLISH_TEST_TOKEN"}}
	cfg.Projects = slices.Clone(cfg.Projects)
	for _, m := range mutate {
		m(cfg)
	}
	cfg.Projects[0] = cfg.Project
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	return &publisherFixture{t: t, s: &Service{cfg: cfg, memory: hearsay.Health{}}, cfg: cfg, repo: repo, hooks: hooks}
}

// pass runs one reconcile pass and returns the requests it made.
func (f *publisherFixture) pass() ([]hooksPost, hearsay.Result) {
	f.t.Helper()
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(f.t, err)
	return f.hooks.take(), res
}

// reopen closes and reopens the project's trace, as a service restart does.
func (f *publisherFixture) reopen() {
	f.t.Helper()
	must(f.t, f.repo.Close())
	repo, err := trace.Open(f.cfg.Root, f.cfg.Project)
	must(f.t, err)
	f.repo = repo
	f.t.Cleanup(func() { repo.Close() })
}

func (f *publisherFixture) recordSeal(stream config.WorkstreamID, n int, specText string) {
	f.t.Helper()
	ctx := context.Background()
	at := fixturekit.DemoStart.Add(time.Duration(n) * time.Hour)
	spec := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "spec", Revision: n, Project: f.repo.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "fixture"}, Path: "spec.md", Content: specText}
	must(f.t, f.repo.RecordDocuments(ctx, []trace.Document{spec}))
	data, err := seal.Encode(seal.Seal{Version: 1, Seal: n, Round: 1, Revision: shed.Pin{Spec: n, Plan: 1}, SpecHash: seal.SpecHash(specText), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "abc"}, Branch: "feature", Workspace: "/owned/feature"})
	must(f.t, err)
	sealDoc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: n, Project: f.repo.Project(), Workstream: stream, At: at.Add(time.Minute), Actor: ownerActor, Cause: "seal"}, Path: seal.Path, Content: string(data)}
	must(f.t, f.repo.RecordDocuments(ctx, []trace.Document{sealDoc}))
}

func (f *publisherFixture) setFeature(stream config.WorkstreamID, id, to, reason string, at time.Time) {
	f.t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: id}
	_, err := f.repo.SetFeatureState(context.Background(), h, to, reason)
	must(f.t, err)
}

func find(posts []hooksPost, kind, field, value string) *hooksPost {
	for i := range posts {
		if posts[i].str("kind") == kind && posts[i].str(field) == value {
			return &posts[i]
		}
	}
	return nil
}

func kinds(posts []hooksPost) []string {
	var out []string
	for _, p := range posts {
		out = append(out, p.Source+":"+p.str("kind")+":"+p.str("document")+p.str("workstream"))
	}
	return out
}

func TestPublishBackfillPostsEveryEligibleArtifactParentsFirst(t *testing.T) {
	f := newPublisherFixture(t)
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	f.recordSeal(fixturekit.Stream, 1, "# Upload limits\n\nCap uploads at 10 MiB.\n")
	f.recordSeal(fixturekit.Stream, 2, "# Upload limits\n\nCap uploads at 20 MiB.\n")

	posts, res := f.pass()
	if res.Posts != len(posts) || len(res.Failures) != 0 {
		t.Fatalf("pass: %+v", res)
	}
	// The project, both workstreams, two charter revisions and two seal
	// revisions of the one spec document.
	if len(posts) != 7 {
		t.Fatalf("%d posts: %v", len(posts), kinds(posts))
	}
	if posts[0].str("kind") != "project" || posts[1].str("kind") != "workstream" || posts[2].str("kind") != "workstream" {
		t.Fatalf("parents were not posted first: %v", kinds(posts))
	}
	for _, p := range posts {
		if p.Auth != "Bearer "+publishTestToken {
			t.Fatalf("post carried %q", p.Auth)
		}
		if p.str("created_at") == "" || p.str("project") != string(f.repo.Project()) {
			t.Fatalf("post: %s", p.Raw)
		}
	}
	project := posts[0]
	if project.Source != "osmia-work" || project.str("author") != "owner" || project.Body["repos"] != nil || project.str("name") == "" {
		t.Fatalf("project: %s", project.Raw)
	}
	stream := find(posts, "workstream", "workstream", string(fixturekit.Stream))
	if stream == nil || stream.Source != "osmia-work" || stream.str("author") != "" || stream.str("state") != "proposed" || stream.str("status") == "" {
		t.Fatalf("workstream: %+v", stream)
	}
	if !strings.Contains(stream.str("description"), "Handed issue title") || stream.str("name") != "Upload limits" {
		t.Fatalf("workstream carries the ask and a title: %s", stream.Raw)
	}
	var charters, specs []hooksPost
	for _, p := range posts {
		if p.str("kind") != "document" {
			continue
		}
		if p.Source != "osmia-owner" || p.str("author") != "owner" {
			t.Fatalf("a document was not posted on the owner source with the owner as author: %s on %s", p.Raw, p.Source)
		}
		if p.str("document") == "charter" {
			charters = append(charters, p)
		} else {
			specs = append(specs, p)
		}
	}
	if len(charters) != 2 || len(specs) != 2 || charters[0].str("workstream") != "" {
		t.Fatalf("documents: %v", kinds(posts))
	}
	if specs[0].str("document") != string(fixturekit.Stream)+"-spec" || specs[0].str("workstream") != string(fixturekit.Stream) || specs[0].str("document") != specs[1].str("document") ||
		!strings.Contains(specs[0].str("body"), "10 MiB") || !strings.Contains(specs[1].str("body"), "20 MiB") {
		t.Fatalf("spec revisions: %v / %v", specs[0].Raw, specs[1].Raw)
	}
	if specs[0].str("created_at") != specs[1].str("created_at") || specs[0].str("updated_at") >= specs[1].str("updated_at") {
		t.Fatalf("spec revision times: %s %s", specs[0].Raw, specs[1].Raw)
	}
	// Records posted in several revisions are recorded at the latest one.
	if again, _ := f.pass(); len(again) != 0 {
		t.Fatalf("the second pass after the backfill posted %v", kinds(again))
	}
}

func TestPublishSecondPassSendsNothingAndAChangePostsOnce(t *testing.T) {
	f := newPublisherFixture(t)
	f.pass()
	if posts, _ := f.pass(); len(posts) != 0 {
		t.Fatalf("an unchanged trace posted %v", kinds(posts))
	}
	// A new charter revision.
	fixturekit.RecordCharter(t, f.repo, trace.CharterTemplate+"\n## Rules\n\n1. Keep the upload API stable.\n2. Cache nothing.\n")
	posts, _ := f.pass()
	if len(posts) != 1 || posts[0].str("document") != "charter" || !strings.Contains(posts[0].str("body"), "Cache nothing") {
		t.Fatalf("charter change posted %v", kinds(posts))
	}
	// A new seal.
	f.recordSeal(fixturekit.Stream, 1, "# Upload limits\n\nCap uploads.\n")
	posts, _ = f.pass()
	if len(posts) != 2 || find(posts, "document", "workstream", string(fixturekit.Stream)) == nil || find(posts, "workstream", "workstream", string(fixturekit.Stream)) == nil {
		// The seal retitles the workstream and adds its spec document.
		t.Fatalf("seal posted %v", kinds(posts))
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("after the seal: %v", kinds(posts))
	}
	// A state change on one workstream.
	f.setFeature(fixturekit.Quiet, "abandon", "abandoned", "no longer needed", fixturekit.DemoStart.Add(24*time.Hour))
	posts, _ = f.pass()
	if len(posts) != 1 || posts[0].str("workstream") != string(fixturekit.Quiet) || posts[0].str("state") != "abandoned" || posts[0].str("status") != "abandoned" || !strings.Contains(posts[0].str("description"), "no longer needed") {
		t.Fatalf("abandonment posted %v: %v", kinds(posts), posts)
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("after the abandonment: %v", kinds(posts))
	}
}

func TestPublishOutageKeepsDigestsAndALaterPassPostsThePending(t *testing.T) {
	f := newPublisherFixture(t)
	f.hooks.setStatus(http.StatusInternalServerError)
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(t, err)
	if res.Posts != 0 || len(res.Failures) == 0 || strings.Contains(fmt.Sprint(res.Failures), publishTestToken) {
		t.Fatalf("pass during the outage: %+v", res)
	}
	if posted, err := f.repo.HearsayPosted(); err != nil || len(posted) != 0 {
		t.Fatalf("digests recorded during the outage: %v %v", posted, err)
	}
	// Local work carries on while Hearsay is down.
	f.setFeature(fixturekit.Stream, "abandon", "abandoned", "dropped", fixturekit.DemoStart.Add(time.Hour))

	f.hooks.setStatus(http.StatusAccepted)
	posts, _ := f.pass()
	if len(posts) != 5 {
		t.Fatalf("recovery posted %v", kinds(posts))
	}
	if find(posts, "workstream", "workstream", string(fixturekit.Stream)).str("state") != "abandoned" {
		t.Fatalf("recovery posted a stale state: %v", posts)
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("after recovery: %v", kinds(posts))
	}
}

func TestPublishUnreachableEndpointRecordsNothing(t *testing.T) {
	f := newPublisherFixture(t)
	f.hooks.Close()
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(t, err)
	if !res.Unreachable || res.Posts != 0 {
		t.Fatalf("pass: %+v", res)
	}
	if posted, err := f.repo.HearsayPosted(); err != nil || len(posted) != 0 {
		t.Fatalf("digests recorded: %v %v", posted, err)
	}
}

func TestPublishChildIsSkippedWhileItsParentIsRefused(t *testing.T) {
	f := newPublisherFixture(t)
	// Only the work source is refused: the project fails, so nothing that
	// names it is attempted, and nothing on the owner source is posted.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/osmia-work") {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		t.Errorf("a record was posted to %s although its parent was refused", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer refused.Close()
	f.cfg.Hearsay.Publish.HooksURL = refused.URL
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(t, err)
	if res.Posts != 0 || len(res.Failures) < 4 {
		t.Fatalf("pass: %+v", res)
	}
}

func TestPublishRecordIDsAreStableAcrossRestartAndRepost(t *testing.T) {
	f := newPublisherFixture(t)
	f.recordSeal(fixturekit.Stream, 1, "# Upload limits\n\nCap uploads.\n")
	first, _ := f.pass()
	f.reopen()
	if posts, _ := f.pass(); len(posts) != 0 {
		t.Fatalf("a restart reposted %v", kinds(posts))
	}
	// Clearing every digest reposts the same artifacts under the same ids.
	posted, err := f.repo.HearsayPosted()
	must(t, err)
	var cleared []trace.PostedRecord
	for _, p := range posted {
		p.Digest = ""
		cleared = append(cleared, p)
	}
	must(t, f.repo.RecordHearsayPosted(context.Background(), cleared, fixturekit.DemoStart))
	second, _ := f.pass()
	if len(second) != len(first) {
		t.Fatalf("a repost sent %v, first sent %v", kinds(second), kinds(first))
	}
	identify := func(p hooksPost) string {
		return p.Source + "|" + p.str("kind") + "|" + p.str("project") + "|" + p.str("workstream") + "|" + p.str("document")
	}
	ids := func(posts []hooksPost) []string {
		var out []string
		for _, p := range posts {
			out = append(out, identify(p))
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(ids(first), ids(second)) {
		t.Fatalf("ids changed:\n%v\n%v", ids(first), ids(second))
	}
	// Identical content reposts byte for byte, so Hearsay writes nothing.
	// A record with several revisions (the charter) is matched revision by
	// revision: its revisions share an id and differ in updated_at.
	revision := func(p hooksPost) string { return identify(p) + "|" + p.str("updated_at") }
	byRevision := map[string]string{}
	for _, p := range first {
		byRevision[revision(p)] = p.Raw
	}
	if len(byRevision) != len(first) {
		t.Fatalf("revisions of one record are not told apart: %v", kinds(first))
	}
	for _, p := range second {
		want, ok := byRevision[revision(p)]
		if !ok || want != p.Raw {
			t.Fatalf("a repost differs:\n%s\n%s", want, p.Raw)
		}
	}
}

func TestPublishProjectDeclaresItsRepoOnlyWhenItOptedIn(t *testing.T) {
	f := newPublisherFixture(t)
	posts, _ := f.pass()
	if _, has := posts[0].Body["repos"]; has {
		t.Fatalf("a default project was posted with repos: %s", posts[0].Raw)
	}
	f = newPublisherFixture(t, func(c *config.Config) { c.Project.HearsayDeclareRepo = true })
	posts, _ = f.pass()
	repos, _ := posts[0].Body["repos"].([]any)
	if len(repos) != 1 || posts[0].str("author") != "owner" {
		t.Fatalf("an opted-in project: %s", posts[0].Raw)
	}
	repo, _ := repos[0].(map[string]any)
	if repo["project"] != f.cfg.Project.Upstream || repo["source"] != "github" {
		t.Fatalf("repos: %v", repos)
	}
}

func TestPublishCarriesPullRequestAndNeverItsBody(t *testing.T) {
	f := newPublisherFixture(t)
	const prBody = "PR-BODY-SECRET-DESCRIPTION"
	record := api.DeliveryPublication{Status: "opened", Upstream: "o/r", Base: "main", Branch: "feature", Commit: "tip", PullRequest: 7, URL: "https://github.com/o/r/pull/7", Title: "Title", Description: prBody}
	data, err := json.Marshal(record)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "final-publication", Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart.Add(time.Hour), Actor: ownerActor, Cause: "fixture"}
	must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "final/publication.json", Content: string(data)}}))
	f.setFeature(fixturekit.Stream, "delivered", "delivered", "delivered", fixturekit.DemoStart.Add(2*time.Hour))

	posts, _ := f.pass()
	stream := find(posts, "workstream", "workstream", string(fixturekit.Stream))
	if stream == nil || stream.str("state") != "completed" || stream.str("status") != "delivered" || stream.str("url") != "https://github.com/o/r/pull/7" || !strings.Contains(stream.str("description"), "(open)") {
		t.Fatalf("delivered workstream: %+v", stream)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, prBody) {
			t.Fatalf("a post carries the pull request body: %s", p.Raw)
		}
	}

	// A recorded merge changes the description and the post is made once.
	out, err := json.Marshal(map[string]any{"pull_request": 7, "outcome": "merged", "merge_commit": "m"})
	must(t, err)
	oh := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "own-pull-request-outcome", Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart.Add(3 * time.Hour), Actor: ownerActor, Cause: "fixture"}
	must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: oh, Path: "own-pull-request-outcome.json", Content: string(out)}}))
	posts, _ = f.pass()
	if len(posts) != 1 || !strings.Contains(posts[0].str("description"), "(merged)") || posts[0].str("state") != "completed" {
		t.Fatalf("merge posted %v", posts)
	}
}

func TestPublishBlockedAndStartedStatesFollowOsmiaStates(t *testing.T) {
	for _, tc := range []struct {
		feature string
		gates   int
		want    string
	}{
		{"", 0, "proposed"}, {"handed", 0, "proposed"}, {"sketched", 0, "proposed"}, {"in-shed", 0, "proposed"}, {"in-shed", 1, "blocked"},
		{"ratified", 0, "started"}, {"building", 0, "started"}, {"building", 1, "blocked"}, {"assembled", 0, "started"},
		{"delivered", 1, "completed"}, {"abandoned", 1, "abandoned"},
	} {
		st := &publishStream{status: trace.WorkstreamStatus{State: tc.feature, Gates: make([]trace.OwnerGate, tc.gates)}}
		if got := st.coreState(); got != tc.want {
			t.Errorf("%q with %d gates is %q, want %q", tc.feature, tc.gates, got, tc.want)
		}
		if tc.feature != "" && st.osmiaState() != tc.feature {
			t.Errorf("status of %q is %q", tc.feature, st.osmiaState())
		}
	}
}

func TestPublishSkipsLibrarianAndSessionWorkstreams(t *testing.T) {
	f := newPublisherFixture(t)
	ctx := context.Background()
	librarian := librarianWorkstream(f.repo.Project())
	must(t, f.repo.CreateWorkstream(ctx, librarian, fixturekit.DemoStart, ownerActor))
	f.recordSeal(librarian, 1, "# Librarian\n")
	session := config.WorkstreamID("w_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	must(t, f.repo.CreateSessionWorkstream(ctx, session, "profile", "base", "branch", "An exploratory session", fixturekit.DemoStart, ownerActor))
	posts, _ := f.pass()
	for _, p := range posts {
		if p.str("workstream") == string(librarian) || p.str("workstream") == string(session) || strings.Contains(p.Raw, "exploratory") {
			t.Fatalf("a post is about the librarian or a session: %s", p.Raw)
		}
	}
	if len(posts) != 5 {
		t.Fatalf("posts: %v", kinds(posts))
	}
}

func TestPublishWorkgroupMembersNameTheGroupAndItsProjectSpecIsPostedOnce(t *testing.T) {
	f := newPublisherFixture(t)
	ctx := context.Background()
	group, err := trace.CreateWorkgroup(ctx, f.repo, []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet}, fixturekit.DemoStart, ownerActor, "test")
	must(t, err)
	posts, _ := f.pass()
	for _, id := range []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet} {
		other := fixturekit.Stream
		if id == other {
			other = fixturekit.Quiet
		}
		w := find(posts, "workstream", "workstream", string(id))
		if w == nil || !strings.Contains(w.str("description"), string(group.ID)) || !strings.Contains(w.str("description"), string(other)) {
			t.Fatalf("a member does not name its workgroup and the other member: %+v", w)
		}
	}
	// A draft project spec is not posted; the approved one is, once.
	for _, p := range posts {
		if strings.Contains(p.str("document"), "project-spec") {
			t.Fatalf("a spec nobody approved was posted: %s", p.Raw)
		}
	}
	spec := trace.ProjectSpec{Behavior: "The two repos agree on one upload contract.", Contracts: []trace.Contract{{Name: "upload-record", Text: "Records carry a size."}}, Graph: []trace.Edge{{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeContract}}, Slices: []trace.Slice{{Workstream: fixturekit.Stream, Spec: "SLICE-SPEC", Plan: "SLICE-PLAN"}}}
	must(t, trace.RecordWorkgroupRevision(ctx, f.repo, group, "approved", spec, fixturekit.DemoStart.Add(time.Hour), ownerActor))
	posts, _ = f.pass()
	if len(posts) != 1 {
		t.Fatalf("approved project spec posted %v", kinds(posts))
	}
	p := posts[0]
	if p.Source != "osmia-owner" || p.str("kind") != "document" || p.str("author") != "owner" || p.str("workstream") != "" || p.str("document") != string(group.ID)+"-project-spec" ||
		!strings.Contains(p.str("body"), "one upload contract") || !strings.Contains(p.str("body"), "upload-record") || strings.Contains(p.Raw, "SLICE-") {
		t.Fatalf("project spec: %s", p.Raw)
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("project spec reposted: %v", kinds(posts))
	}
}

func TestPublishLoopSendsNothingWithoutPublishConfiguration(t *testing.T) {
	// serial: sets the publisher's token in the process environment.
	t.Setenv("PUBLISH_TEST_TOKEN", publishTestToken)
	hooks := newHooksFake(t)
	_, cfg := conversationFixture(t, "hearsay-unpublished-")
	cfg.Hearsay = config.Hearsay{URL: hooks.URL, Principal: "owner", TokenEnv: "PUBLISH_TEST_OWNER", Agents: map[string]config.HearsayAgent{
		"worker": {ID: "w", TokenEnv: "W"}, "orchestrator": {ID: "o", TokenEnv: "O"}, "observer": {ID: "b", TokenEnv: "B"}}}
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	s := &Service{cfg: cfg, memory: hearsay.Health{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.publishHearsay(ctx, repo) }()
	time.Sleep(300 * time.Millisecond)
	// Local work progresses meanwhile.
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandon", Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "abandon"}
	_, err = repo.SetFeatureState(context.Background(), h, "abandoned", "done")
	must(t, err)
	time.Sleep(100 * time.Millisecond)
	cancel()
	must(t, <-done)
	if posts := hooks.take(); len(posts) != 0 {
		t.Fatalf("unconfigured publishing posted %v", kinds(posts))
	}
}

func TestPublishLoopPostsWhilePublishingIsConfiguredAndStopsWithTheProject(t *testing.T) {
	f := newPublisherFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.publishHearsay(ctx, f.repo) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		posted, err := f.repo.HearsayPosted()
		must(t, err)
		// One digest per artifact: the project, two workstreams and the charter,
		// whose revisions share a digest.
		if len(posted) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the loop recorded %d digests", len(posted))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	must(t, <-done)
	// Every artifact reached the hooks endpoint; the charter's revisions are
	// posted separately.
	if posts := f.hooks.take(); len(posts) < 4 {
		t.Fatalf("the loop posted %v", kinds(posts))
	}
}
