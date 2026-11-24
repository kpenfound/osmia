package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const publishTestToken = "publisher-token-8a7b6c5d"

type publisherFixture struct {
	t     *testing.T
	s     *Service
	cfg   *config.Config
	repo  *trace.Repository
	hooks *fixturekit.Hooks
}

func newPublisherFixture(t *testing.T, mutate ...func(*config.Config)) *publisherFixture {
	t.Helper()
	t.Setenv("PUBLISH_TEST_TOKEN", publishTestToken)
	hooks := fixturekit.NewHooks(t)
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
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	return &publisherFixture{t: t, s: &Service{cfg: cfg, memory: hearsay.Health{}}, cfg: cfg, repo: repo, hooks: hooks}
}

func (f *publisherFixture) pass() ([]fixturekit.HooksPost, hearsay.Result) {
	f.t.Helper()
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	fixturekit.Must(f.t, err)
	return f.hooks.Take(), res
}

func (f *publisherFixture) reopen() {
	f.t.Helper()
	fixturekit.Must(f.t, f.repo.Close())
	repo, err := trace.Open(f.cfg.Root, f.cfg.Project)
	fixturekit.Must(f.t, err)
	f.repo = repo
	f.t.Cleanup(func() { repo.Close() })
}

func (f *publisherFixture) recordSeal(stream config.WorkstreamID, n int, specText string) {
	f.t.Helper()
	ctx := context.Background()
	at := fixturekit.DemoStart.Add(time.Duration(n) * time.Hour)
	spec := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "spec", Revision: n, Project: f.repo.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "fixture"}, Path: "spec.md", Content: specText}
	fixturekit.Must(f.t, f.repo.RecordDocuments(ctx, []trace.Document{spec}))
	data, err := seal.Encode(seal.Seal{Version: 1, Seal: n, Round: 1, Revision: shed.Pin{Spec: n, Plan: 1}, SpecHash: seal.SpecHash(specText), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "abc"}, Branch: "feature", Workspace: "/owned/feature"})
	fixturekit.Must(f.t, err)
	sealDoc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: n, Project: f.repo.Project(), Workstream: stream, At: at.Add(time.Minute), Actor: ownerActor, Cause: "seal"}, Path: seal.Path, Content: string(data)}
	fixturekit.Must(f.t, f.repo.RecordDocuments(ctx, []trace.Document{sealDoc}))
}

func (f *publisherFixture) setFeature(stream config.WorkstreamID, id, to, reason string, at time.Time) {
	f.t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: id}
	_, err := f.repo.SetFeatureState(context.Background(), h, to, reason)
	fixturekit.Must(f.t, err)
}

func find(posts []fixturekit.HooksPost, kind, field, value string) *fixturekit.HooksPost {
	for i := range posts {
		if posts[i].Str("kind") == kind && posts[i].Str(field) == value {
			return &posts[i]
		}
	}
	return nil
}

func kinds(posts []fixturekit.HooksPost) []string {
	var out []string
	for _, p := range posts {
		out = append(out, p.Source+":"+p.Str("kind")+":"+p.Str("document")+p.Str("workstream"))
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
	if len(posts) != 7 {
		t.Fatalf("%d posts: %v", len(posts), kinds(posts))
	}
	if posts[0].Str("kind") != "project" || posts[1].Str("kind") != "workstream" || posts[2].Str("kind") != "workstream" {
		t.Fatalf("parents were not posted first: %v", kinds(posts))
	}
	for _, p := range posts {
		if p.Auth != "Bearer "+publishTestToken {
			t.Fatalf("post carried %q", p.Auth)
		}
		if p.Str("created_at") == "" || p.Str("project") != string(f.repo.Project()) {
			t.Fatalf("post: %s", p.Raw)
		}
	}
	project := posts[0]
	if project.Source != "osmia-work" || project.Str("author") != "owner" || project.Body["repos"] != nil || project.Str("name") == "" {
		t.Fatalf("project: %s", project.Raw)
	}
	stream := find(posts, "workstream", "workstream", string(fixturekit.Stream))
	if stream == nil || stream.Source != "osmia-work" || stream.Str("author") != "" || stream.Str("state") != "proposed" || stream.Str("status") == "" {
		t.Fatalf("workstream: %+v", stream)
	}
	if !strings.Contains(stream.Str("description"), "Handed issue title") || stream.Str("name") != "Upload limits" {
		t.Fatalf("workstream carries the ask and a title: %s", stream.Raw)
	}
	var charters, specs []fixturekit.HooksPost
	for _, p := range posts {
		if p.Str("kind") != "document" {
			continue
		}
		if p.Source != "osmia-owner" || p.Str("author") != "owner" {
			t.Fatalf("a document was not posted on the owner source with the owner as author: %s on %s", p.Raw, p.Source)
		}
		if p.Str("document") == "charter" {
			charters = append(charters, p)
		} else {
			specs = append(specs, p)
		}
	}
	if len(charters) != 2 || len(specs) != 2 || charters[0].Str("workstream") != "" {
		t.Fatalf("documents: %v", kinds(posts))
	}
	if specs[0].Str("document") != string(fixturekit.Stream)+"-spec" || specs[0].Str("workstream") != string(fixturekit.Stream) || specs[0].Str("document") != specs[1].Str("document") ||
		!strings.Contains(specs[0].Str("body"), "10 MiB") || !strings.Contains(specs[1].Str("body"), "20 MiB") {
		t.Fatalf("spec revisions: %v / %v", specs[0].Raw, specs[1].Raw)
	}
	if specs[0].Str("created_at") != specs[1].Str("created_at") || specs[0].Str("updated_at") >= specs[1].Str("updated_at") {
		t.Fatalf("spec revision times: %s %s", specs[0].Raw, specs[1].Raw)
	}
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
	fixturekit.RecordCharter(t, f.repo, trace.CharterTemplate+"\n## Rules\n\n1. Keep the upload API stable.\n2. Cache nothing.\n")
	posts, _ := f.pass()
	if len(posts) != 1 || posts[0].Str("document") != "charter" || !strings.Contains(posts[0].Str("body"), "Cache nothing") {
		t.Fatalf("charter change posted %v", kinds(posts))
	}
	f.recordSeal(fixturekit.Stream, 1, "# Upload limits\n\nCap uploads.\n")
	posts, _ = f.pass()
	if len(posts) != 2 || find(posts, "document", "workstream", string(fixturekit.Stream)) == nil || find(posts, "workstream", "workstream", string(fixturekit.Stream)) == nil {
		t.Fatalf("seal posted %v", kinds(posts))
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("after the seal: %v", kinds(posts))
	}
	f.setFeature(fixturekit.Quiet, "abandon", "abandoned", "no longer needed", fixturekit.DemoStart.Add(24*time.Hour))
	posts, _ = f.pass()
	if len(posts) != 1 || posts[0].Str("workstream") != string(fixturekit.Quiet) || posts[0].Str("state") != "abandoned" || posts[0].Str("status") != "abandoned" || !strings.Contains(posts[0].Str("description"), "no longer needed") {
		t.Fatalf("abandonment posted %v: %v", kinds(posts), posts)
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("after the abandonment: %v", kinds(posts))
	}
}

func TestPublishOutageKeepsDigestsAndALaterPassPostsThePending(t *testing.T) {
	f := newPublisherFixture(t)
	f.hooks.SetStatus(http.StatusInternalServerError)
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	fixturekit.Must(t, err)
	if res.Posts != 0 || len(res.Failures) == 0 || strings.Contains(fmt.Sprint(res.Failures), publishTestToken) {
		t.Fatalf("pass during the outage: %+v", res)
	}
	if posted, err := f.repo.HearsayPosted(); err != nil || len(posted) != 0 {
		t.Fatalf("digests recorded during the outage: %v %v", posted, err)
	}
	f.setFeature(fixturekit.Stream, "abandon", "abandoned", "dropped", fixturekit.DemoStart.Add(time.Hour))

	f.hooks.SetStatus(http.StatusAccepted)
	posts, _ := f.pass()
	if len(posts) != 5 {
		t.Fatalf("recovery posted %v", kinds(posts))
	}
	if find(posts, "workstream", "workstream", string(fixturekit.Stream)).Str("state") != "abandoned" {
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
	fixturekit.Must(t, err)
	if !res.Unreachable || res.Posts != 0 {
		t.Fatalf("pass: %+v", res)
	}
	if posted, err := f.repo.HearsayPosted(); err != nil || len(posted) != 0 {
		t.Fatalf("digests recorded: %v %v", posted, err)
	}
}

func TestPublishChildIsSkippedWhileItsParentIsRefused(t *testing.T) {
	f := newPublisherFixture(t)
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
	fixturekit.Must(t, err)
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
	posted, err := f.repo.HearsayPosted()
	fixturekit.Must(t, err)
	var cleared []trace.PostedRecord
	for _, p := range posted {
		p.Digest = ""
		cleared = append(cleared, p)
	}
	fixturekit.Must(t, f.repo.RecordHearsayPosted(context.Background(), cleared, fixturekit.DemoStart))
	second, _ := f.pass()
	if len(second) != len(first) {
		t.Fatalf("a repost sent %v, first sent %v", kinds(second), kinds(first))
	}
	identify := func(p fixturekit.HooksPost) string {
		return p.Source + "|" + p.Str("kind") + "|" + p.Str("project") + "|" + p.Str("workstream") + "|" + p.Str("document")
	}
	ids := func(posts []fixturekit.HooksPost) []string {
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
	revision := func(p fixturekit.HooksPost) string { return identify(p) + "|" + p.Str("updated_at") }
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
	if len(repos) != 1 || posts[0].Str("author") != "owner" {
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
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "final-publication", Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart.Add(time.Hour), Actor: ownerActor, Cause: "fixture"}
	fixturekit.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "final/publication.json", Content: string(data)}}))
	f.setFeature(fixturekit.Stream, "delivered", "delivered", "delivered", fixturekit.DemoStart.Add(2*time.Hour))

	posts, _ := f.pass()
	stream := find(posts, "workstream", "workstream", string(fixturekit.Stream))
	if stream == nil || stream.Str("state") != "completed" || stream.Str("status") != "delivered" || stream.Str("url") != "https://github.com/o/r/pull/7" || !strings.Contains(stream.Str("description"), "(open)") {
		t.Fatalf("delivered workstream: %+v", stream)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, prBody) {
			t.Fatalf("a post carries the pull request body: %s", p.Raw)
		}
	}

	out, err := json.Marshal(map[string]any{"pull_request": 7, "outcome": "merged", "merge_commit": "m"})
	fixturekit.Must(t, err)
	oh := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "own-pull-request-outcome", Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart.Add(3 * time.Hour), Actor: ownerActor, Cause: "fixture"}
	fixturekit.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: oh, Path: "own-pull-request-outcome.json", Content: string(out)}}))
	posts, _ = f.pass()
	if len(posts) != 1 || !strings.Contains(posts[0].Str("description"), "(merged)") || posts[0].Str("state") != "completed" {
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

func TestPublishSkipsSessionWorkstreams(t *testing.T) {
	f := newPublisherFixture(t)
	ctx := context.Background()
	session := config.WorkstreamID("w_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fixturekit.Must(t, f.repo.CreateSessionWorkstream(ctx, session, "profile", "base", "branch", "An exploratory session", fixturekit.DemoStart, ownerActor))
	posts, _ := f.pass()
	for _, p := range posts {
		if p.Str("workstream") == string(session) || strings.Contains(p.Raw, "exploratory") {
			t.Fatalf("a post is about a session: %s", p.Raw)
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
	fixturekit.Must(t, err)
	posts, _ := f.pass()
	for _, id := range []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet} {
		other := fixturekit.Stream
		if id == other {
			other = fixturekit.Quiet
		}
		w := find(posts, "workstream", "workstream", string(id))
		if w == nil || !strings.Contains(w.Str("description"), string(group.ID)) || !strings.Contains(w.Str("description"), string(other)) {
			t.Fatalf("a member does not name its workgroup and the other member: %+v", w)
		}
	}
	for _, p := range posts {
		if strings.Contains(p.Str("document"), "project-spec") {
			t.Fatalf("a spec nobody approved was posted: %s", p.Raw)
		}
	}
	spec := trace.ProjectSpec{Behavior: "The two repos agree on one upload contract.", Contracts: []trace.Contract{{Name: "upload-record", Text: "Records carry a size."}}, Graph: []trace.Edge{{From: fixturekit.Stream, To: fixturekit.Quiet, Type: trace.EdgeContract}}, Slices: []trace.Slice{{Workstream: fixturekit.Stream, Spec: "SLICE-SPEC", Plan: "SLICE-PLAN"}}}
	fixturekit.Must(t, trace.RecordWorkgroupRevision(ctx, f.repo, group, "approved", spec, fixturekit.DemoStart.Add(time.Hour), ownerActor))
	posts, _ = f.pass()
	if len(posts) != 1 {
		t.Fatalf("approved project spec posted %v", kinds(posts))
	}
	p := posts[0]
	if p.Source != "osmia-owner" || p.Str("kind") != "document" || p.Str("author") != "owner" || p.Str("workstream") != "" || p.Str("document") != string(group.ID)+"-project-spec" ||
		!strings.Contains(p.Str("body"), "one upload contract") || !strings.Contains(p.Str("body"), "upload-record") || strings.Contains(p.Raw, "SLICE-") {
		t.Fatalf("project spec: %s", p.Raw)
	}
	if posts, _ = f.pass(); len(posts) != 0 {
		t.Fatalf("project spec reposted: %v", kinds(posts))
	}
}

func TestPublishLoopSendsNothingWithoutPublishConfiguration(t *testing.T) {
	t.Setenv("PUBLISH_TEST_TOKEN", publishTestToken)
	hooks := fixturekit.NewHooks(t)
	_, cfg := conversationFixture(t, "hearsay-unpublished-")
	cfg.Hearsay = config.Hearsay{URL: hooks.URL, Principal: "owner", TokenEnv: "PUBLISH_TEST_OWNER", Agents: map[string]config.HearsayAgent{
		"worker": {ID: "w", TokenEnv: "W"}, "orchestrator": {ID: "o", TokenEnv: "O"}, "observer": {ID: "b", TokenEnv: "B"}}}
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandon", Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "abandon"}
	_, err = repo.SetFeatureState(context.Background(), h, "abandoned", "done")
	fixturekit.Must(t, err)
	iterated := make(chan struct{})
	s := &Service{cfg: cfg, memory: hearsay.Health{}, options: Options{hearsayPassed: func() { close(iterated) }}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.publishHearsay(ctx, repo) }()
	intakefixture.Await(t, "the publishing loop's first iteration", iterated)
	cancel()
	fixturekit.Must(t, <-done)
	if posts := hooks.Take(); len(posts) != 0 {
		t.Fatalf("unconfigured publishing posted %v", kinds(posts))
	}
}

func TestPublishLoopPostsWhilePublishingIsConfiguredAndStopsWithTheProject(t *testing.T) {
	f := newPublisherFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.publishHearsay(ctx, f.repo) }()
	intakefixture.Soon(t, "the loop to record its digests", func() bool {
		posted, err := f.repo.HearsayPosted()
		fixturekit.Must(t, err)
		return len(posted) >= 4
	})
	cancel()
	fixturekit.Must(t, <-done)
	if posts := f.hooks.Take(); len(posts) < 4 {
		t.Fatalf("the loop posted %v", kinds(posts))
	}
}
