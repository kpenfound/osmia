package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *publisherFixture) restart() {
	f.t.Helper()
	f.s = &Service{cfg: f.s.current(), memory: hearsay.Health{}}
	if f.repo != nil {
		f.reopen()
	}
}

func (f *publisherFixture) cleanup(stream config.WorkstreamID) {
	f.t.Helper()
	ctx := context.Background()
	intakefixture.Must(f.t, f.s.keepForCleanup(ctx, f.repo, stream))
	moved, err := f.repo.MarkWorkstreamTraceForDeletion(stream)
	intakefixture.Must(f.t, err)
	if !moved {
		f.t.Fatalf("the trace of %s was not moved aside", stream)
	}
	intakefixture.Must(f.t, trace.FinishWorkstreamTraceDeletion(f.cfg.Root, f.repo.Project(), stream))
}

func (f *publisherFixture) keptKeys() []string {
	f.t.Helper()
	kept, err := f.repo.HearsayKept()
	intakefixture.Must(f.t, err)
	var out []string
	for _, k := range kept {
		if !k.Released {
			out = append(out, k.Source+"/"+k.Key)
		}
	}
	slices.Sort(out)
	return out
}

func (f *publisherFixture) recordMerge(stream config.WorkstreamID) {
	f.t.Helper()
	record := api.DeliveryPublication{Status: "opened", Upstream: "o/r", Base: "main", Branch: "feature", Commit: "tip", PullRequest: 7, URL: "https://github.com/o/r/pull/7", Title: "Title", Description: "PR-BODY-NEVER-SENT"}
	data, err := json.Marshal(record)
	intakefixture.Must(f.t, err)
	f.document(stream, "final-publication", 1, "final/publication.json", string(data), hr(10), ownerActor)
	out, err := json.Marshal(map[string]any{"pull_request": 7, "outcome": "merged", "merge_commit": "m"})
	intakefixture.Must(f.t, err)
	f.document(stream, "own-pull-request-outcome", 1, "own-pull-request-outcome.json", string(out), hr(11), ownerActor)
	f.setFeature(stream, "delivered", "delivered", "delivered", hr(12))
}

func (f *publisherFixture) decideWork(stream config.WorkstreamID) {
	f.t.Helper()
	f.recordSeal(stream, 1, "# Upload limits\n\nCap uploads at 10 MiB.\n")
	f.ask(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(3))
	f.escalate(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(4))
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Cap uploads at 10 MiB."}, hr(5), trace.Actor{Kind: "owner", ID: "local"})
	content := fmt.Sprintf(`{"version":1,"assumptions":[{"workstreams":[%q],"id":"1","text":"Treat the 10 MiB cap as inclusive.","actor":{"kind":"agent","id":"chief_of_staff"},"thread":"chief","turn":"t1","at":%q}]}`, string(stream), hr(6).Format(time.RFC3339))
	f.document(stream, trace.AssumptionsDocument, 1, trace.AssumptionsPath, content, hr(6), chiefActor)
}

func (f *publisherFixture) down() (recover func()) {
	f.t.Helper()
	addr := f.hooks.Listener.Addr().String()
	f.hooks.Close()
	return func() { f.hooks = newHooksFakeOn(f.t, addr) }
}

func postsAbout(posts []hooksPost, kind, field, value string) []hooksPost {
	var out []hooksPost
	for _, p := range posts {
		if p.str("kind") == kind && p.str(field) == value {
			out = append(out, p)
		}
	}
	return out
}

func TestCleanupKeepsPendingPublicationThroughAnOutageAndARestart(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.decideWork(stream)
	f.recordMerge(stream)
	recoverHooks := f.down()

	if posts, res := f.pass(); len(posts) != 0 || !res.Unreachable {
		t.Fatalf("a pass during the outage: %v %+v", posts, res)
	}
	f.cleanup(stream)
	if posted, err := f.repo.HearsayPosted(); err != nil || len(posted) != 0 {
		t.Fatalf("cleanup recorded digests while Hearsay was down: %v %v", posted, err)
	}
	wantKept := []string{
		"osmia-owner/decision/" + string(stream) + "-ruling-1",
		"osmia-owner/document/" + string(stream) + "-spec",
		"osmia-work/decision/assumption-",
		"osmia-work/workstream/" + string(stream),
	}
	kept := f.keptKeys()
	if len(kept) != len(wantKept) {
		t.Fatalf("kept %v, want %v", kept, wantKept)
	}
	for i, want := range wantKept {
		if !strings.HasPrefix(kept[i], want) {
			t.Fatalf("kept %v, want %v", kept, wantKept)
		}
	}
	if posts, _ := f.pass(); len(posts) != 0 {
		t.Fatalf("posted while down: %v", kinds(posts))
	}

	f.restart()
	recoverHooks()
	posts, res := f.pass()
	if len(res.Failures) != 0 || res.Unreachable {
		t.Fatalf("recovery pass: %+v", res)
	}
	ws := postsAbout(posts, "workstream", "workstream", string(stream))
	if len(ws) != 1 || ws[0].Source != "osmia-work" || ws[0].str("author") != "" || ws[0].str("state") != "completed" || ws[0].str("status") != "delivered" ||
		ws[0].str("url") != "https://github.com/o/r/pull/7" || !strings.Contains(ws[0].str("description"), "(merged)") {
		t.Fatalf("final state: %v", ws)
	}
	spec := postsAbout(posts, "document", "document", string(stream)+"-spec")
	if len(spec) != 1 || spec[0].Source != "osmia-owner" || spec[0].str("author") != "owner" || !strings.Contains(spec[0].str("body"), "Cap uploads at 10 MiB.") {
		t.Fatalf("seal document: %v", spec)
	}
	ruling := postsAbout(posts, "decision", "decision", string(stream)+"-ruling-1")
	if len(ruling) != 1 || ruling[0].Source != "osmia-owner" || ruling[0].str("author") != "owner" || ruling[0].str("decider") != "owner" || ruling[0].str("outcome") != "Cap uploads at 10 MiB." {
		t.Fatalf("ruling: %v", ruling)
	}
	var assumptions []hooksPost
	for _, d := range decisions(posts) {
		if strings.Contains(d.str("decision"), "assumption") {
			assumptions = append(assumptions, d)
		}
	}
	if len(assumptions) != 1 || assumptions[0].Source != "osmia-work" || assumptions[0].str("author") != "" || assumptions[0].str("decider") != "chief_of_staff" || assumptions[0].str("workstream") != string(stream) {
		t.Fatalf("assumption: %v", assumptions)
	}
	for _, p := range posts {
		if strings.Contains(p.Raw, "PR-BODY-NEVER-SENT") {
			t.Fatalf("a post carries the pull request body: %s", p.Raw)
		}
	}
	if left := f.keptKeys(); len(left) != 0 {
		t.Fatalf("kept after they were posted: %v", left)
	}
	for range 2 {
		if again, _ := f.pass(); len(again) != 0 {
			t.Fatalf("reposted: %v", kinds(again))
		}
		f.restart()
	}
}

// The same records reach Hearsay under the same ids and sources whether the
// workstream's trace was cleaned in between or not.
func TestKeptRecordsPostWhatTheLiveTraceWould(t *testing.T) {
	post := func(clean bool) []hooksPost {
		f := newPublisherFixture(t)
		stream := fixturekit.Stream
		fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
		f.decideWork(stream)
		f.recordMerge(stream)
		if clean {
			f.cleanup(stream)
		}
		posts, _ := f.pass()
		return posts
	}
	shape := func(posts []hooksPost) []string {
		var out []string
		for _, p := range posts {
			if p.str("workstream") != string(fixturekit.Stream) {
				continue
			}
			p.Body["project"] = ""
			delete(p.Body, "updated_at")
			delete(p.Body, "created_at")
			raw, _ := json.Marshal(p.Body)
			out = append(out, p.Source+" "+string(raw))
		}
		slices.Sort(out)
		return out
	}
	live, kept := shape(post(false)), shape(post(true))
	if len(live) != 4 || !slices.Equal(live, kept) {
		t.Fatalf("live:\n%s\nkept:\n%s", strings.Join(live, "\n"), strings.Join(kept, "\n"))
	}
}

func TestCleanupKeepsAnAbandonedWorkstreamWithItsReason(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.decideWork(stream)
	f.setFeature(stream, "abandon", "abandoned", "No longer needed.", hr(9))
	recoverHooks := f.down()
	f.cleanup(stream)

	f.restart()
	recoverHooks()
	posts, _ := f.pass()
	ws := postsAbout(posts, "workstream", "workstream", string(stream))
	if len(ws) != 1 || ws[0].str("state") != "abandoned" || ws[0].str("status") != "abandoned" || !strings.Contains(ws[0].str("description"), "Cancelled: No longer needed.") {
		t.Fatalf("abandoned state: %v", ws)
	}
	if len(postsAbout(posts, "document", "document", string(stream)+"-spec")) != 1 || len(decisions(posts)) != 2 {
		t.Fatalf("pending records: %v", kinds(posts))
	}
	for range 2 {
		if again, _ := f.pass(); len(again) != 0 {
			t.Fatalf("reposted: %v", kinds(again))
		}
		f.restart()
	}
}

func TestCleanupKeepsOnlyWhatIsNotPostedAtItsCurrentDigest(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.recordSeal(stream, 1, "# Upload limits\n\nCap uploads at 10 MiB.\n")
	if posts, _ := f.pass(); len(posts) == 0 {
		t.Fatal("nothing was posted")
	}
	f.cleanup(stream)
	if kept := f.keptKeys(); len(kept) != 0 {
		t.Fatalf("kept %v although everything was posted", kept)
	}
	if posts, _ := f.pass(); len(posts) != 0 {
		t.Fatalf("posted after cleanup: %v", kinds(posts))
	}

	g := newPublisherFixture(t)
	fixturekit.RecordHanded(t, g.repo, stream, "stdin", fixturekit.DemoStart)
	g.recordSeal(stream, 1, "# Upload limits\n\nCap uploads at 10 MiB.\n")
	g.pass()
	g.recordSeal(stream, 2, "# Upload limits\n\nCap uploads at 20 MiB.\n")
	recoverHooks := g.down()
	g.cleanup(stream)
	kept, err := g.repo.HearsayKept()
	intakefixture.Must(t, err)
	var doc hearsay.Record
	for _, k := range kept {
		if strings.HasSuffix(k.Key, "-spec") {
			intakefixture.Must(t, json.Unmarshal(k.Data, &doc))
		}
	}
	if len(doc.Versions) != 1 || !strings.Contains(doc.Versions[0].Body, "20 MiB") {
		t.Fatalf("kept spec revisions: %+v", doc.Versions)
	}
	g.restart()
	recoverHooks()
	posts, _ := g.pass()
	if spec := postsAbout(posts, "document", "document", string(stream)+"-spec"); len(spec) != 1 || !strings.Contains(spec[0].str("body"), "20 MiB") {
		t.Fatalf("recovery posted %v", kinds(posts))
	}
}

func TestKeptRecordsHoldOnlyEligibleContent(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.decideWork(stream)
	f.recordMerge(stream)
	f.ask(stream, "2", "Which function retries?", hr(7))
	f.ruling(stream, "2", 1, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "CODE-ANSWER retry lives in client.go.", Citations: []string{"inspection#inspection-abc"}}, hr(8), chiefActor)
	f.amendment(stream, "a1", []string{"spec#1"}, "UNDECIDED-AMENDMENT-CHANGE", "UNDECIDED-AMENDMENT-REASON", "", hr(9))
	f.document(stream, "unit-report", 1, "units/u1/report.json", `{"learnings":["MASON-LEARNING-SENTINEL"]}`, hr(9), trace.Actor{Kind: "agent", ID: "mason"})
	recoverHooks := f.down()
	f.cleanup(stream)

	docs, err := trace.Read[trace.Document](f.repo, "")
	intakefixture.Must(t, err)
	n := 0
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, "hearsay-kept-") {
			continue
		}
		n++
		for _, banned := range []string{"CODE-ANSWER", "UNDECIDED-AMENDMENT", "MASON-LEARNING-SENTINEL", "PR-BODY-NEVER-SENT", publishTestToken} {
			if strings.Contains(d.Content, banned) {
				t.Fatalf("a kept record holds %q: %s", banned, d.Content)
			}
		}
	}
	if n != 4 {
		t.Fatalf("%d kept documents, want the workstream, its spec and its two decisions", n)
	}
	f.restart()
	recoverHooks()
	posts, _ := f.pass()
	for _, p := range posts {
		for _, banned := range []string{"CODE-ANSWER", "UNDECIDED-AMENDMENT", "MASON-LEARNING-SENTINEL", "PR-BODY-NEVER-SENT"} {
			if strings.Contains(p.Raw, banned) {
				t.Fatalf("a post holds %q: %s", banned, p.Raw)
			}
		}
	}
}

func TestCleanupBeforePublishingWasConfiguredLeavesNothingToBackfill(t *testing.T) {
	f := newPublisherFixture(t, func(c *config.Config) { c.Hearsay.Publish = nil })
	stream := fixturekit.Stream
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.decideWork(stream)
	f.recordMerge(stream)
	before := documentCount(t, f.repo)
	f.cleanup(stream)
	if after := documentCount(t, f.repo); after != before {
		t.Fatalf("cleanup wrote %d documents while publishing was off", after-before)
	}

	f.cfg.Hearsay.Publish = &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: f.hooks.URL, Agent: "osmia-publisher", TokenEnv: "PUBLISH_TEST_TOKEN"}
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	intakefixture.Must(t, err)
	posts := f.hooks.take()
	if len(res.Failures) != 0 || len(posts) == 0 {
		t.Fatalf("backfill: %+v %v", res, kinds(posts))
	}
	for _, p := range posts {
		if p.str("workstream") == string(stream) || p.str("kind") == "decision" || strings.Contains(p.Raw, string(stream)) {
			t.Fatalf("the cleaned workstream was posted: %s", p.Raw)
		}
	}
}

func documentCount(t *testing.T, repo *trace.Repository) int {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	intakefixture.Must(t, err)
	return len(docs)
}

func TestRemovalIsKeptAndPostedOnceAfterARestartWithTheProjectInactive(t *testing.T) {
	f := newPublisherFixture(t)
	stream := fixturekit.Stream
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.decideWork(stream)
	recoverHooks := f.down()
	project := f.repo.Project()

	if err := f.s.keepForRemoval(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	intakefixture.Must(t, f.repo.Close())
	f.s.cfg = f.cfg.WithoutProjectID(project)
	f.repo = nil
	if !f.s.publishRemovedPass(context.Background()) {
		t.Fatal("an unreachable Hearsay did not back the removed project off")
	}

	f.restart()
	recoverHooks()
	if f.s.current().Active(project) {
		t.Fatal("the project is active after the restart")
	}
	if f.s.publishRemovedPass(context.Background()) {
		t.Fatal("recovery pass asked to back off")
	}
	posts := f.hooks.take()
	removals := postsAbout(posts, "project", "project", string(project))
	if len(removals) != 1 || removals[0].Source != "osmia-work" || removals[0].str("status") != "removed" || removals[0].str("state") != "abandoned" || removals[0].str("author") != "owner" || removals[0].Body["repos"] != nil {
		t.Fatalf("removal: %v", removals)
	}
	if posts[0].str("kind") != "project" {
		t.Fatalf("order: %v", kinds(posts))
	}
	for _, want := range []string{"workstream", "document", "decision"} {
		if len(postsAbout(posts, want, "project", string(project))) == 0 {
			t.Fatalf("no %s was posted with the removal: %v", want, kinds(posts))
		}
	}
	if len(postsAbout(posts, "decision", "project", string(project))) != 2 {
		t.Fatalf("decisions: %v", kinds(posts))
	}
	for range 2 {
		f.s.publishRemovedPass(context.Background())
		if again := f.hooks.take(); len(again) != 0 {
			t.Fatalf("reposted: %v", kinds(again))
		}
		f.s = &Service{cfg: f.s.current(), memory: hearsay.Health{}}
	}
}

func TestRemovalOfAPostedProjectPostsOnlyTheRemoval(t *testing.T) {
	f := newPublisherFixture(t)
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	f.pass()
	project := f.repo.Project()
	recoverHooks := f.down()
	intakefixture.Must(t, f.s.keepForRemoval(context.Background(), f.repo))
	intakefixture.Must(t, f.repo.Close())
	f.s.cfg = f.cfg.WithoutProjectID(project)
	f.repo = nil
	f.restart()
	recoverHooks()
	f.s.publishRemovedPass(context.Background())
	posts := f.hooks.take()
	if len(posts) != 1 || posts[0].str("kind") != "project" || posts[0].str("status") != "removed" {
		t.Fatalf("removal posted %v", kinds(posts))
	}
	f.s.publishRemovedPass(context.Background())
	if again := f.hooks.take(); len(again) != 0 {
		t.Fatalf("reposted: %v", kinds(again))
	}
}

func TestRemovalWritesNothingWithoutPublishing(t *testing.T) {
	f := newPublisherFixture(t, func(c *config.Config) { c.Hearsay.Publish = nil })
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	before := documentCount(t, f.repo)
	intakefixture.Must(t, f.s.keepForRemoval(context.Background(), f.repo))
	intakefixture.Must(t, f.s.keepForCleanup(context.Background(), f.repo, fixturekit.Stream))
	if after := documentCount(t, f.repo); after != before {
		t.Fatalf("%d documents were written", after-before)
	}
	f.s.cfg = f.cfg.WithoutProjectID(f.repo.Project())
	intakefixture.Must(t, f.repo.Close())
	if f.s.publishRemovedPass(context.Background()) || len(f.hooks.take()) != 0 {
		t.Fatal("a removed project was published without publishing configured")
	}
}

func TestRemovedProjectIsPostedByTheServiceAfterARestart(t *testing.T) {
	t.Setenv("PUBLISH_TEST_TOKEN", publishTestToken)
	opts, clone := projectFixture(t)
	hooks := newHooksFake(t)
	addr := hooks.Listener.Addr().String()
	hooks.Close()
	path := filepath.Join(opts.Config.Root, "config.toml")
	text, err := os.ReadFile(path)
	intakefixture.Must(t, err)
	text = append(text, []byte(fmt.Sprintf(`
[hearsay]
url = "https://memory.example"
principal = "owner"
token_env = "PUBLISH_TEST_OWNER"

[hearsay.agents.worker]
id = "osmia-worker"
token_env = "PUBLISH_TEST_WORKER"

[hearsay.agents.orchestrator]
id = "osmia-chief"
token_env = "PUBLISH_TEST_CHIEF"

[hearsay.agents.observer]
id = "osmia-observer"
token_env = "PUBLISH_TEST_OBSERVER"

[hearsay.publish]
work_source = "osmia-work"
owner_source = "osmia-owner"
hooks_url = "http://%s"
agent = "osmia-publisher"
token_env = "PUBLISH_TEST_TOKEN"
`, addr))...)
	intakefixture.Must(t, os.WriteFile(path, text, 0600))

	ctx := context.Background()
	var passes atomic.Int64
	opts.hearsayPassed = func() { passes.Add(1) }
	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	c := NewClient(s.Socket())
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	intakefixture.Must(t, err)
	id := added.Project.ID
	started := time.Now()
	_, err = c.RemoveProject(ctx, id)
	intakefixture.Must(t, err)
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("removal took %s with Hearsay down", took)
	}
	c.Close()
	intakefixture.Must(t, s.Close())

	hooks = newHooksFakeOn(t, addr)
	passes.Store(0)
	s, err = Start(ctx, opts)
	intakefixture.Must(t, err)
	t.Cleanup(func() { s.Close() })
	var posts []hooksPost
	intakefixture.Soon(t, "the removal to be posted", func() bool {
		posts = append(posts, hooks.take()...)
		return len(postsAbout(posts, "project", "status", "removed")) > 0
	})
	intakefixture.Soon(t, "the removal pass to finish", func() bool { return passes.Load() > 0 })
	posts = append(posts, hooks.take()...)
	if removals := postsAbout(posts, "project", "project", string(id)); len(removals) != 1 || removals[0].str("state") != "abandoned" {
		t.Fatalf("removals: %v", kinds(posts))
	}
	if charter := postsAbout(posts, "document", "document", "charter"); len(charter) == 0 {
		t.Fatalf("the project's pending charter was not posted with it: %v", kinds(posts))
	}
	intakefixture.Must(t, s.Close())
	passes.Store(0)
	s, err = Start(ctx, opts)
	intakefixture.Must(t, err)
	t.Cleanup(func() { s.Close() })
	intakefixture.Soon(t, "the removal pass to finish", func() bool { return passes.Load() > 0 })
	if again := hooks.take(); len(again) != 0 {
		t.Fatalf("reposted after another restart: %v", kinds(again))
	}
}
