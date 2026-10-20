package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// gatedLanding pauses an admitted landing before its local step starts.
// Independent workstreams may enter while the first still awaits release.
type gatedLanding struct {
	coreadapter.Reconciler
	entered  chan string
	finished chan string
	releases sync.Map
}

func (g *gatedLanding) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	g.entered <- op.ID
	release, _ := g.releases.Load(op.ID)
	select {
	case <-release.(chan struct{}):
	case <-ctx.Done():
		return coreadapter.OperationResult{}, ctx.Err()
	}
	result, err := g.Reconciler.Apply(ctx, op)
	if err == nil {
		g.finished <- op.ID
	}
	return result, err
}

func awaitGitOperation(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(demoTimeout):
		t.Fatal("repository operation did not reach its gate")
		return ""
	}
}

func approvedStream(t *testing.T, f *shedFixture, repo *trace.Repository, key, backend string) config.WorkstreamID {
	t.Helper()
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream := recordBuild(t, f, repo, key, independentPlan, backend, base)
	seedReview(t, f, repo, stream, "resume", resumeReport)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	return stream
}

func TestWorkstreamLandingsRunConcurrently(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, a, repo := newApprovedFixtureOn(t, "concurrent-a", backend)
			b := approvedStream(t, f, repo, "concurrent-b", backend)
			lands := &foreman{masons: newMasonController(f.s, repo)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			must(t, lands.Pass(ctx))
			gate := &gatedLanding{Reconciler: lands, entered: make(chan string, 3), finished: make(chan string, 3)}
			add := func(stream config.WorkstreamID) string {
				t.Helper()
				ops := landOperations(t, repo, stream)
				if len(ops) != 1 {
					t.Fatalf("stream %s has %d pending landings", stream, len(ops))
				}
				id := ops[0].Operation.ID
				ch := make(chan struct{})
				gate.releases.Store(id, ch)
				t.Cleanup(func() {
					select {
					case <-ch:
					default:
						close(ch)
					}
				})
				return id
			}
			first, second := add(a), add(b)
			controller, err := reconcile.New(repo, reconcile.Options{Worker: "landing-fixture", Schedule: lands.Pass,
				Concurrent: concurrentOperation, Class: operationClass, Limit: f.s.gitOperationLimit(repo, nil),
				Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: gate},
				Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != LandAction },
			})
			must(t, err)
			defer func() { cancel(); must(t, controller.Wait()) }()
			must(t, controller.Pass(ctx))
			entered := []string{awaitGitOperation(t, gate.entered), awaitGitOperation(t, gate.entered)}
			if !slices.Contains(entered, first) || !slices.Contains(entered, second) {
				t.Fatalf("concurrent landings %v, want %s and %s", entered, first, second)
			}
			// B finishes while A remains admitted and blocked.
			ch, _ := gate.releases.Load(second)
			close(ch.(chan struct{}))
			if id := awaitGitOperation(t, gate.finished); id != second {
				t.Fatalf("finished %s, want %s", id, second)
			}
			// A's in-flight effect must not prevent a new C landing request.
			c := approvedStream(t, f, repo, "concurrent-c", backend)
			must(t, lands.Pass(ctx))
			third := add(c)
			must(t, controller.Pass(ctx))
			if id := awaitGitOperation(t, gate.entered); id != third {
				t.Fatalf("started %s, want independent landing %s", id, third)
			}
			ch, _ = gate.releases.Load(third)
			close(ch.(chan struct{}))
			if id := awaitGitOperation(t, gate.finished); id != third {
				t.Fatalf("finished %s, want %s", id, third)
			}
			state, err := repo.Workflow(a, trace.UnitSubject("resume"))
			must(t, err)
			if state.Value != UnitApproved {
				t.Fatalf("held landing moved its unit to %s", state.Value)
			}
			ch, _ = gate.releases.Load(first)
			close(ch.(chan struct{}))
			must(t, controller.Wait())
			for _, stream := range []config.WorkstreamID{a, b, c} {
				ops := landOperations(t, repo, stream)
				if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
					t.Fatalf("landing %s: %+v", stream, ops)
				}
				_, review := approvedReview(t, repo, stream, "resume")
				if commits := f.landedCommits(t, stream, review.Identity.Candidate.BaseRevision); len(commits) != 1 {
					t.Fatalf("feature branch %s has %v", featureBranch(stream), commits)
				}
			}
		})
	}
}

func TestLandingWaitsOnlyForItsWorkstreamsRebasesAndRefresh(t *testing.T) {
	t.Parallel()
	for _, wait := range []string{"rebase", "refresh"} {
		t.Run(wait, func(t *testing.T) {
			t.Parallel()
			f, a, repo := newApprovedFixture(t, "waiting-a")
			b := approvedStream(t, f, repo, "waiting-b", config.WorkspacesGit)
			lands := &foreman{masons: newMasonController(f.s, repo)}
			if wait == "rebase" {
				moveFeature(t, f, a, map[string]string{"OTHER.md": "new feature tip\n"})
			} else {
				queueGitFixtureOperation(t, repo, librarianWorkstream(repo.Project()), RefreshAction, coreadapter.RunnerBoundary,
					refreshInput{Workstream: a, Commit: strings.Repeat("a", 40), Unit: "resume", Landing: 1, Report: 1})
			}
			must(t, lands.Pass(context.Background()))
			if ops := landOperations(t, repo, a); len(ops) != 0 {
				t.Fatalf("held workstream received landing: %+v", ops)
			}
			if ops := landOperations(t, repo, b); len(ops) != 1 {
				t.Fatalf("independent workstream has %d landings", len(ops))
			}
			if wait == "rebase" && len(rebaseOperations(t, repo, a, "resume")) != 1 {
				t.Fatal("the workstream behind its feature branch was not rebased")
			}
		})
	}
}

func queueGitFixtureOperation(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string, boundary coreadapter.OperationBoundary, input any) coreadapter.Operation {
	t.Helper()
	data, err := json.Marshal(input)
	must(t, err)
	id := "fixture-" + action
	event := trace.EventID(id, "run")
	op := coreadapter.Operation{ID: trace.OperationID(repo.Project(), stream, event), Boundary: boundary, Action: action, Input: data}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: time.Now().UTC(), Actor: foremanActor, Cause: "fixture"},
		Subject: id, To: "requested", Reason: "fixture requests an operation"}, Events: []trace.Event{{ID: event, Kind: action, Body: "Run fixture operation", Operation: &op}}})
	must(t, err)
	return op
}

type capacityGitEffects struct {
	entered chan string
	release <-chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func (e *capacityGitEffects) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	active := e.active.Add(1)
	defer e.active.Add(-1)
	for peak := e.peak.Load(); active > peak; peak = e.peak.Load() {
		if e.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	e.entered <- op.ID
	select {
	case <-e.release:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "fixture effect absent"}, nil
	case <-ctx.Done():
		return coreadapter.Observation{}, ctx.Err()
	}
}
func (*capacityGitEffects) Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error) {
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: "fixture effect completed"}, nil
}

func TestGitOperationLimitLeavesExcessPendingAndAdmitsModelWork(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			t.Parallel()
			f, _, repo := newApprovedFixture(t, "capacity")
			f.s.cfg.Project.Capacity.GitOperations = limit
			var streams []config.WorkstreamID
			actions := []string{SealAction, BuildAction, LandAction, RebaseAction, DriftAction, PublishAction, publishUpstreamAction}
			for _, action := range append(slices.Clone(actions), "model-fixture") {
				stream, err := config.NewWorkstreamID()
				must(t, err)
				must(t, repo.CreateWorkstreamOn(context.Background(), stream, config.WorkspacesGit, time.Now().UTC(), ownerActor))
				streams = append(streams, stream)
				boundary := coreadapter.RepositoryBoundary
				if action == "model-fixture" {
					boundary = coreadapter.RunnerBoundary
				}
				queueGitFixtureOperation(t, repo, stream, action, boundary, map[string]string{})
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			effects := &capacityGitEffects{entered: make(chan string, 8), release: release}
			controller, err := reconcile.New(repo, reconcile.Options{Worker: "capacity-fixture", Workstreams: func() ([]config.WorkstreamID, error) { return streams, nil },
				Concurrent: concurrentOperation, Class: operationClass, Limit: f.s.gitOperationLimit(repo, nil),
				Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: effects, coreadapter.RunnerBoundary: completedEffect{}}})
			must(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); releaseOnce.Do(func() { close(release) }); must(t, controller.Wait()) }()
			must(t, controller.Pass(ctx))
			want := limit
			if want == 0 {
				want = 4
			}
			for range want {
				awaitGitOperation(t, effects.entered)
			}
			claimed := 0
			for _, stream := range streams[:len(actions)] {
				ops, err := repo.Operations(stream)
				must(t, err)
				if len(ops[0].History) > 0 {
					claimed++
				}
			}
			if claimed != want {
				t.Fatalf("claimed %d Git operations, want %d", claimed, want)
			}
			model, err := repo.Operations(streams[len(actions)])
			must(t, err)
			if !model[0].Acknowledged {
				t.Fatal("Git capacity blocked model work or the pass")
			}
			releaseOnce.Do(func() { close(release) })
			must(t, controller.Wait())
			for range len(actions) {
				must(t, controller.Pass(ctx))
				must(t, controller.Wait())
			}
			for _, stream := range streams {
				ops, err := repo.Operations(stream)
				must(t, err)
				if !ops[0].Acknowledged {
					t.Fatalf("pending operation never ran: %+v", ops)
				}
			}
			if peak := int(effects.peak.Load()); peak > want {
				t.Fatalf("peak %d exceeds limit %d", peak, want)
			}
		})
	}
}

func TestDriftRequestsWaitOnlyForTheirOwnLanding(t *testing.T) {
	t.Parallel()
	f, a, repo := newApprovedFixture(t, "landing-before-drift")
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	b := recordBuild(t, f, repo, "independent-drift", independentPlan, config.WorkspacesGit, base)
	lands := &foreman{masons: newMasonController(f.s, repo)}
	must(t, lands.Pass(context.Background()))
	if len(landOperations(t, repo, a)) != 1 {
		t.Fatal("fixture has no pending landing")
	}
	requested, err := lands.requestDrifts(context.Background(), testDrift)
	must(t, err)
	if !slices.Equal(requested, []config.WorkstreamID{b}) {
		t.Fatalf("drift requests %v, want only independent workstream %s", requested, b)
	}
}

func TestGitCapacitySharesPollPermitsAndAdoptsLimits(t *testing.T) {
	t.Parallel()
	f, _, repo := newApprovedFixture(t, "shared-git-capacity")
	setLimit := func(limit int) {
		cfg := *f.s.cfg
		cfg.Project.Capacity.GitOperations = limit
		f.s.mu.Lock()
		f.s.cfg = &cfg
		f.s.mu.Unlock()
	}
	setLimit(2)
	poll, ok := f.s.tryGitOperation(repo)
	if !ok {
		t.Fatal("poll was not admitted")
	}
	defer poll()
	var otherUsed int
	other := func(coreadapter.Operation) (func(), bool) {
		otherUsed++
		return func() { otherUsed-- }, true
	}
	limit := f.s.gitOperationLimit(repo, other)
	op := coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: SealAction}
	workflow, ok := limit(op)
	if !ok {
		t.Fatal("workflow was not admitted alongside poll")
	}
	defer workflow()
	if _, ok := limit(op); ok || otherUsed != 1 {
		t.Fatalf("full pool admitted work or leaked another permit: used %d", otherUsed)
	}
	// New admission observes reloads without forgetting active poll/workflow work.
	setLimit(1)
	if _, ok := limit(op); ok {
		t.Fatal("reduced capacity forgot active operations")
	}
	workflow()
	if _, ok := limit(op); ok {
		t.Fatal("reduced capacity ignored the active poll")
	}
	setLimit(2)
	next, ok := limit(op)
	if !ok {
		t.Fatal("increased capacity did not admit work")
	}
	next()
	poll()
	model, ok := limit(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: DraftAction})
	if !ok {
		t.Fatal("Git capacity held a model operation")
	}
	model()
	if otherUsed != 0 {
		t.Fatalf("other limit leaked %d permits", otherUsed)
	}
}

func TestBusyGitWorkstreamDoesNotWakeItself(t *testing.T) {
	t.Parallel()
	f, _, repo := newApprovedFixture(t, "busy-git-admission")
	stream, err := config.NewWorkstreamID()
	must(t, err)
	must(t, repo.CreateWorkstreamOn(context.Background(), stream, config.WorkspacesGit, f.s.now(), ownerActor))
	queueGitFixtureOperation(t, repo, stream, SealAction, coreadapter.RepositoryBoundary, map[string]string{})
	queueGitFixtureOperation(t, repo, stream, BuildAction, coreadapter.RepositoryBoundary, map[string]string{})
	ops, err := repo.Operations(stream)
	must(t, err)
	attempt, _, err := repo.BeginOperation(context.Background(), stream, ops[0].EventID, trace.WorkflowOperation, foremanActor, f.s.now)
	must(t, err)
	if attempt == nil {
		t.Fatal("fixture did not reserve its workstream")
	}
	defer attempt.Close()
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "busy-git-fixture",
		Workstreams: func() ([]config.WorkstreamID, error) { return []config.WorkstreamID{stream}, nil },
		Concurrent:  concurrentOperation, Class: operationClass, Limit: f.s.gitOperationLimit(repo, nil),
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: completedEffect{}},
	})
	must(t, err)
	defer func() { must(t, controller.Wait()) }()
	// Consume the coalesced durable-write hint before trying busy admission.
	repo.WakeWorkflow()
	must(t, repo.WaitWorkflow(context.Background(), nil))
	must(t, controller.Pass(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := repo.WaitWorkflow(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy admission woke another pass: %v", err)
	}
	// Poll completions do wake admission, since they finish outside the controller.
	release, admitted := f.s.tryGitOperation(repo)
	if !admitted {
		t.Fatal("busy workflow admission leaked a Git permit")
	}
	release()
	ctx, cancelWake := context.WithTimeout(context.Background(), demoTimeout)
	defer cancelWake()
	must(t, repo.WaitWorkflow(ctx, nil))
}

func TestGitWorkflowAdmissionHonorsPauseAndBaseWait(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "git-holds")
	f.s.options.PullRequests = &fakePulls{}
	store, _, err := runtime.Open(runtime.Inputs{Config: f.s.cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{repo.Project(): {stream}}})
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	f.s.store = store
	target := runtime.Target{Scope: "workstream", Project: repo.Project(), Workstream: stream}
	must(t, f.s.store.SetPause(runtime.Pause{Target: target, Mode: "soft", Source: runtime.PauseOwner, Reason: "owner pause", SetAt: f.s.now()}))
	hold := f.s.holding(repo)
	actions := []string{SealAction, BuildAction, LandAction, RebaseAction, DriftAction, PublishAction, publishUpstreamAction}
	for _, action := range actions {
		want := slices.Contains([]string{LandAction, RebaseAction, PublishAction, publishUpstreamAction}, action)
		if got := hold(stream, coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: action}); got != want {
			t.Fatalf("paused %s: held %t, want %t", action, got, want)
		}
	}
	must(t, f.s.store.ClearPause(target, "owner"))
	for _, action := range actions {
		if hold(stream, coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: action}) {
			t.Fatalf("resumed %s remains held", action)
		}
	}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-base-wait", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: foremanActor, Cause: "fixture"},
		Subject: baseWaitSubject, To: "waiting", Reason: "the base is unavailable",
	}})
	must(t, err)
	for _, action := range actions {
		if !hold(stream, coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: action}) {
			t.Fatalf("base wait admitted %s", action)
		}
	}
}
