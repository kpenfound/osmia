package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type landPort struct{ *router.Router }

func (p landPort) Submit(x router.Exit, done func(router.Result, error)) bool {
	res, err := p.Route(context.Background(), x)
	if done != nil {
		done(res, err)
	}
	return true
}

type landFixture struct {
	t        *testing.T
	cfg      *config.Config
	repo     *trace.Repository
	home     string
	upstream string
	scratch  string
	pulls    *fixturekit.FakePulls
	router   *router.Router
	clock    *watchClock

	base, reviewed, signed string
	branch                 string

	options landOptions

	mu       sync.Mutex
	steps    []string
	hooks    map[string]func() error
	rebases  int
	moved    func() string
	findings []FinalFinding
}

type landOptions struct {
	checks               string
	delivery             string
	edgeWait, mergeOrder string
}

func newLandFixture(t *testing.T, opts ...landOptions) *landFixture {
	t.Helper()
	var o landOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	cfg, repo := deliveryTrace(t)
	home := filepath.Dir(cfg.Project.Clone)
	f := &landFixture{t: t, cfg: cfg, repo: repo, home: home, clock: &watchClock{at: fixturekit.DemoStart.Add(time.Hour)}, hooks: map[string]func() error{}, branch: featureBranch(stream), options: o}
	cfg.Project.Delivery = config.DeliveryJev
	if o.delivery != "" {
		cfg.Project.Delivery = o.delivery
	}
	cfg.Project.Upstream, cfg.Project.Fork, cfg.Project.BaseBranch = "acme/widgets", "", "main"
	f.upstream = filepath.Join(home, "remotes", "acme", "widgets.git")
	f.scratch = filepath.Join(home, "scratch")
	clone := cfg.Project.Clone
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(f.upstream), 0700))
	f.git("init", "--quiet", "--bare", "-b", "main", f.upstream)
	f.git("-C", clone, "checkout", "--quiet", "-B", "main")
	f.write(clone, "README", "widgets\n")
	f.commit(clone, "base")
	f.git("-C", clone, "remote", "add", "upstream", f.upstream)
	f.git("-C", clone, "push", "--quiet", "upstream", "main:main")
	f.git("clone", "--quiet", f.upstream, f.scratch)
	f.base = f.rev(clone, "HEAD")

	f.write(clone, "unit-a.txt", "a\n")
	f.commit(clone, "unit a")
	f.write(clone, "unit-b.txt", "b\n")
	f.commit(clone, "unit b")
	f.reviewed = f.rev(clone, "HEAD")
	f.signed = f.sign(f.reviewed)
	f.git("-C", clone, "update-ref", "refs/heads/"+f.branch, f.signed)
	f.git("-C", clone, "checkout", "--quiet", "main")
	f.git("-C", clone, "push", "--quiet", "upstream", f.signed+":refs/heads/"+f.branch)

	f.pulls = &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 7, URL: "https://github.com/acme/widgets/pull/7", State: "open", Draft: true, Head: f.branch, HeadRepository: "acme/widgets", HeadCommit: f.signed, Base: "main", Title: "Draft", Body: "Draft body\n"}}}
	f.router = &router.Router{Judge: jevfixture.TransitionJudge(f.clock.Now, &jevfixture.Transitions{}), Effects: &router.Effects{}, Now: f.clock.Now}

	f.transition("assembled", "", AssembledState)
	f.record(1, f.reviewed, f.base, f.signed, o.checks)
	return f
}

func (f *landFixture) controller() *Controller {
	return New(Dependencies{
		Now: f.clock.Now,
		Step: func(name string) error {
			f.mu.Lock()
			f.steps = append(f.steps, name)
			hook := f.hooks[name]
			f.mu.Unlock()
			if hook != nil {
				return hook()
			}
			return nil
		},
		About:      func(*trace.Repository) *config.Config { return f.cfg },
		Effective:  func() (runtime.State, []runtime.Diagnostic) { return runtime.State{}, nil },
		ChangeHeld: func(*trace.Repository, config.WorkstreamID) (bool, error) { return false, nil },
		FinalGate: func(_ context.Context, repo *trace.Repository, stream config.WorkstreamID) (FinalReport, string, error) {
			report, found, err := LatestFinalReport(repo, stream)
			if err != nil || !found {
				return report, "no final review is recorded", err
			}
			return report, "", nil
		},
		ConversationTrace: func(string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *api.APIError) {
			return f.repo.Project(), stream, f.repo, nil
		},
		Router: landPort{f.router}, PullRequests: f.pulls,
		UnresolvedWork:      func(*trace.Repository, config.WorkstreamID) (bool, error) { return false, nil },
		EdgePublicationWait: func(*trace.Repository, config.WorkstreamID) (string, error) { return f.options.edgeWait, nil },
		UnresolvedMergeOrderEdge: func(*trace.Repository, config.WorkstreamID) (string, bool, error) {
			return f.options.mergeOrder, f.options.mergeOrder != "", nil
		},
		PendingMergeOrderReferences: func(*trace.Repository, config.WorkstreamID) ([]string, error) { return nil, nil },
		RequestRebase: func(context.Context, *trace.Repository, config.WorkstreamID) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.rebases++
			return nil
		},
	})
}

func (f *landFixture) lander() *Lander { return &Lander{S: f.controller(), Repository: f.repo} }

func (f *landFixture) hook(name string, fn func() error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[name] = func() error {
		f.mu.Lock()
		delete(f.hooks, name)
		f.mu.Unlock()
		return fn()
	}
}

func (f *landFixture) stepCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.steps {
		if s == name {
			n++
		}
	}
	return n
}

func (f *landFixture) git(args ...string) string {
	f.t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(f.t, f.home, args...))
}

func (f *landFixture) rev(dir, rev string) string {
	f.t.Helper()
	return f.git("-C", dir, "rev-parse", rev)
}

func (f *landFixture) write(dir, name, content string) {
	f.t.Helper()
	fixturekit.Must(f.t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0600))
	f.git("-C", dir, "add", name)
}

func (f *landFixture) commit(dir, message string) {
	f.t.Helper()
	f.git("-C", dir, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "--quiet", "-m", message)
}

func (f *landFixture) sign(reviewed string) string {
	f.t.Helper()
	clone := f.cfg.Project.Clone
	return f.git("-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit-tree", reviewed+"^{tree}", "-p", reviewed+"^", "-m", "unit b, signed")
}

func (f *landFixture) advanceUpstream(name string) string {
	f.t.Helper()
	f.git("-C", f.scratch, "fetch", "--quiet", "origin")
	f.git("-C", f.scratch, "reset", "--quiet", "--hard", "origin/main")
	f.write(f.scratch, name, name+"\n")
	f.commit(f.scratch, name)
	f.git("-C", f.scratch, "push", "--quiet", "origin", "main")
	return f.rev(f.scratch, "HEAD")
}

func (f *landFixture) upstreamMain() string { return f.rev(f.upstream, "refs/heads/main") }

func (f *landFixture) upstreamBranch() string { return f.rev(f.upstream, "refs/heads/"+f.branch) }

func (f *landFixture) refuseBase() {
	f.t.Helper()
	hook := filepath.Join(f.upstream, "hooks", "pre-receive")
	script := "#!/bin/sh\nwhile read old new ref; do\n  if [ \"$ref\" = refs/heads/main ]; then\n    echo 'remote: error: GH006: Protected branch update failed for refs/heads/main.' >&2\n    exit 1\n  fi\ndone\n"
	fixturekit.Must(f.t, os.WriteFile(hook, []byte(script), 0700))
}

func (f *landFixture) transition(id, from, to string) {
	f.t.Helper()
	state, err := f.repo.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(f.t, err)
	_, err = f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-" + id, Revision: 1, Project: f.repo.Project(), Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"},
		Subject: trace.FeatureSubject, From: from, To: to, Reason: "fixture"}})
	fixturekit.Must(f.t, err)
}

func (f *landFixture) record(k int, reviewed, base, signed, checks string) {
	f.t.Helper()
	ctx := context.Background()
	report := FinalReport{Review: k, Operation: fmt.Sprintf("review-%d", k), Outcome: FinalReviewed, Branch: f.branch, Before: reviewed, Commit: reviewed, Upstream: &seal.Base{Remote: "upstream", Branch: "main", Commit: base},
		Seal: 1, SpecHash: "spec-hash", Spec: 1, Plan: 1, Summary: "Add the units", Criteria: []FinalCriterion{}, Findings: f.findings}
	content, err := json.Marshal(report)
	fixturekit.Must(f.t, err)
	revision, err := nextRevision(f.repo, stream, FinalReportDocument)
	fixturekit.Must(f.t, err)
	fixturekit.Must(f.t, f.repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalReportDocument, Revision: revision, Project: f.repo.Project(), Workstream: stream, At: f.clock.Now(), Actor: foremanActor, Cause: report.Operation}, Path: FinalReportPath, Content: string(content)}}))
	publisher := &Publisher{S: f.controller(), Repository: f.repo}
	fixturekit.Must(f.t, publisher.RecordPublication(ctx, stream, DeliveryPublication{Push: k, Review: k, Operation: report.Operation, Status: PublicationOpened, Reviewed: reviewed, Commit: signed, PullRequest: 7, URL: "https://github.com/acme/widgets/pull/7",
		Upstream: "acme/widgets", Fork: "acme/widgets", Branch: f.branch, Base: "main", Draft: true, PushedAt: f.clock.Now(), Remote: "upstream", Style: "commit-per-unit"}))
	if checks == "none" {
		return
	}
	run := FinalCheckRun{Review: k, Operation: report.Operation, Commit: reviewed, Source: api.CheckSourcePullRequest, PullRequest: 7, Push: k, PushedCommit: signed, Status: checkrun.ChecksPassed, StartedAt: f.clock.Now(), FinishedAt: f.clock.Now()}
	switch checks {
	case "failed":
		run.Status, run.Failed = checkrun.ChecksFailed, []string{"unit"}
	case "other":
		run.Commit, run.PushedCommit = f.base, f.base
	}
	data, err := json.Marshal(run)
	fixturekit.Must(f.t, err)
	revision, err = nextRevision(f.repo, stream, FinalChecksDocument(k))
	fixturekit.Must(f.t, err)
	fixturekit.Must(f.t, f.repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalChecksDocument(k), Revision: revision, Project: f.repo.Project(), Workstream: stream, At: f.clock.Now(), Actor: foremanActor, Cause: report.Operation}, Path: FinalChecksPath(k), Content: string(data)}}))
}

func (f *landFixture) rebased(k int, onto string) (reviewed, signed string) {
	f.t.Helper()
	return f.rebasedWith(k, onto, "passed")
}

func (f *landFixture) rebasedWith(k int, onto, checks string) (reviewed, signed string) {
	f.t.Helper()
	clone := f.cfg.Project.Clone
	f.git("-C", clone, "fetch", "--quiet", "upstream", "main")
	f.git("-C", clone, "checkout", "--quiet", "--detach", onto)
	f.git("-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "cherry-pick", f.reviewed+"~1", f.reviewed)
	reviewed = f.rev(clone, "HEAD")
	signed = f.sign(reviewed)
	f.git("-C", clone, "update-ref", "refs/heads/"+f.branch, signed)
	f.git("-C", clone, "checkout", "--quiet", "main")
	f.git("-C", clone, "push", "--quiet", "--force", "upstream", signed+":refs/heads/"+f.branch)
	f.record(k, reviewed, onto, signed, checks)
	f.pulls.Mu.Lock()
	f.pulls.Prs[0].HeadCommit = signed
	f.pulls.Mu.Unlock()
	return reviewed, signed
}

func (f *landFixture) target(c *Controller) landingPush {
	f.t.Helper()
	target, reason, err := c.landingTarget(context.Background(), f.repo, f.cfg, stream, true, false)
	if err != nil || reason != "" {
		f.t.Fatalf("the landing target: %q, %v", reason, err)
	}
	return target
}

func (f *landFixture) land(decider LandDecider) LandingResult {
	f.t.Helper()
	c := f.controller()
	result, err := c.landPush(context.Background(), f.repo, stream, LandRequest{Decider: decider, AcceptBlocking: true}, f.target(c))
	fixturekit.Must(f.t, err)
	return result
}

func (f *landFixture) retry() {
	f.t.Helper()
	fixturekit.Must(f.t, f.lander().RetryPass(context.Background()))
}

func (f *landFixture) feature() string {
	f.t.Helper()
	state, err := f.repo.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(f.t, err)
	return state.Value
}

func (f *landFixture) subject(name string) string {
	f.t.Helper()
	state, err := f.repo.Workflow(stream, name)
	fixturekit.Must(f.t, err)
	return state.Value
}

func (f *landFixture) documents(id string) int {
	f.t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, stream)
	fixturekit.Must(f.t, err)
	n := 0
	for _, d := range docs {
		if d.ID == id {
			n++
		}
	}
	return n
}

func (f *landFixture) transitions(id string) []trace.Transition {
	f.t.Helper()
	all, err := trace.Read[trace.Transition](f.repo, stream)
	fixturekit.Must(f.t, err)
	var out []trace.Transition
	for _, t := range all {
		if t.ID == id {
			out = append(out, t)
		}
	}
	return out
}

func (f *landFixture) notices(contains string) int {
	f.t.Helper()
	entries, err := f.repo.Outbox(stream)
	fixturekit.Must(f.t, err)
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Event.Body, contains) {
			n++
		}
	}
	return n
}

func (f *landFixture) operation(n int) coreadapter.Operation {
	f.t.Helper()
	op, err := f.lander().operation(stream, n)
	fixturekit.Must(f.t, err)
	return op
}

func (f *landFixture) busy() {
	f.t.Helper()
	event := trace.EventID("fixture-busy", "run")
	op := coreadapter.Operation{ID: trace.OperationID(f.repo.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: FinalReviewAction, Input: json.RawMessage(`{}`)}
	state, err := f.repo.Workflow(stream, "fixture-busy")
	fixturekit.Must(f.t, err)
	_, err = f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-busy", Revision: 1, Project: f.repo.Project(), Workstream: stream, At: f.clock.Now(), Actor: foremanActor, Cause: "fixture"}, Subject: "fixture-busy", From: state.Value, To: "requested", Reason: "fixture"},
		Events:     []trace.Event{{ID: event, Kind: FinalReviewAction, Body: "a final review", Operation: &op}}})
	fixturekit.Must(f.t, err)
}

func (f *landFixture) finishBusy() {
	f.t.Helper()
	ctx := context.Background()
	event := trace.EventID("fixture-busy", "run")
	a, _, err := f.repo.BeginOperation(ctx, stream, event, trace.WorkerOperation, foremanActor, f.clock.Now)
	fixturekit.Must(f.t, err)
	if a == nil {
		f.t.Fatal("the busy operation cannot be claimed")
	}
	defer a.Close()
	for _, step := range []string{"observe", "effect", "result"} {
		action := a.Action(step, f.clock.Now())
		switch step {
		case "observe":
			action.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "nothing yet"}
		case "result":
			action.Result = &coreadapter.OperationResult{Outcome: "succeeded", Evidence: "done"}
		}
		fixturekit.Must(f.t, a.Record(ctx, action))
	}
}

func (f *landFixture) landingAttempts() int {
	f.t.Helper()
	ops, err := f.repo.Operations(stream)
	fixturekit.Must(f.t, err)
	return len(landingOperationsOf(ops))
}
