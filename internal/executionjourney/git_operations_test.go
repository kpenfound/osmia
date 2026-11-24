package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

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
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("repository operation did not reach its gate")
		return ""
	}
}

func approvedStream(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, key, backend string) config.WorkstreamID {
	t.Helper()
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream := journey.RecordBuild(t, f, repo, key, intakefixture.IndependentPlan, backend, base)
	journey.SeedReview(t, f, repo, stream, "resume", journey.ResumeReport)
	journey.ApproveDirectly(t, f.S, repo, stream, "resume", "spec#1")
	return stream
}

func TestWorkstreamLandingsRunConcurrently(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, a, repo := journey.NewApprovedFixtureOn(t, "concurrent-a", backend)
			b := approvedStream(t, f, repo, "concurrent-b", backend)
			lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repo)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixturekit.Must(t, lands.Pass(ctx))
			gate := &gatedLanding{Reconciler: lands, entered: make(chan string, 3), finished: make(chan string, 3)}
			add := func(stream config.WorkstreamID) string {
				t.Helper()
				ops := journey.LandOperations(t, repo, stream)
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
				Concurrent: service.ConcurrentOperation, Class: service.TestOperationClass, Limit: journey.InspectForTest(f.S).GitOperationLimit(repo, nil),
				Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: gate},
				Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != service.LandAction },
			})
			fixturekit.Must(t, err)
			defer func() { cancel(); fixturekit.Must(t, controller.Wait()) }()
			fixturekit.Must(t, controller.Pass(ctx))
			entered := []string{awaitGitOperation(t, gate.entered), awaitGitOperation(t, gate.entered)}
			if !slices.Contains(entered, first) || !slices.Contains(entered, second) {
				t.Fatalf("concurrent landings %v, want %s and %s", entered, first, second)
			}

			ch, _ := gate.releases.Load(second)
			close(ch.(chan struct{}))
			if id := awaitGitOperation(t, gate.finished); id != second {
				t.Fatalf("finished %s, want %s", id, second)
			}

			c := approvedStream(t, f, repo, "concurrent-c", backend)
			fixturekit.Must(t, lands.Pass(ctx))
			third := add(c)
			fixturekit.Must(t, controller.Pass(ctx))
			if id := awaitGitOperation(t, gate.entered); id != third {
				t.Fatalf("started %s, want independent landing %s", id, third)
			}
			ch, _ = gate.releases.Load(third)
			close(ch.(chan struct{}))
			if id := awaitGitOperation(t, gate.finished); id != third {
				t.Fatalf("finished %s, want %s", id, third)
			}
			state, err := repo.Workflow(a, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value != intake.UnitApproved {
				t.Fatalf("held landing moved its unit to %s", state.Value)
			}
			ch, _ = gate.releases.Load(first)
			close(ch.(chan struct{}))
			fixturekit.Must(t, controller.Wait())
			for _, stream := range []config.WorkstreamID{a, b, c} {
				ops := journey.LandOperations(t, repo, stream)
				if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
					t.Fatalf("landing %s: %+v", stream, ops)
				}
				_, review := journey.ApprovedReview(t, repo, stream, "resume")
				if commits := landedCommits(f, t, stream, review.Identity.Candidate.BaseRevision); len(commits) != 1 {
					t.Fatalf("feature branch %s has %v", intake.FeatureBranch(stream), commits)
				}
			}
		})
	}
}

func TestLandingWaitsOnlyForItsWorkstreamsRebases(t *testing.T) {
	t.Parallel()
	f, a, repo := journey.NewApprovedFixture(t, "waiting-a")
	b := approvedStream(t, f, repo, "waiting-b", config.WorkspacesGit)
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repo)}
	journey.MoveFeature(t, f, a, map[string]string{"OTHER.md": "new feature tip\n"})
	fixturekit.Must(t, lands.Pass(context.Background()))
	if ops := journey.LandOperations(t, repo, a); len(ops) != 0 {
		t.Fatalf("workstream %s has %d landings, want 0: %+v", a, len(ops), ops)
	}
	if ops := journey.LandOperations(t, repo, b); len(ops) != 1 {
		t.Fatalf("independent workstream has %d landings", len(ops))
	}
	if len(journey.RebaseOperations(t, repo, a, "resume")) != 1 {
		t.Fatal("the workstream behind its feature branch was not rebased")
	}
}

func queueGitFixtureOperation(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string, boundary coreadapter.OperationBoundary, input any) coreadapter.Operation {
	t.Helper()
	data, err := json.Marshal(input)
	fixturekit.Must(t, err)
	id := "fixture-" + action
	event := trace.EventID(id, "run")
	op := coreadapter.Operation{ID: trace.OperationID(repo.Project(), stream, event), Boundary: boundary, Action: action, Input: data}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: time.Now().UTC(), Actor: execution.ForemanActor, Cause: "fixture"},
		Subject: id, To: "requested", Reason: "fixture requests an operation"}, Events: []trace.Event{{ID: event, Kind: action, Body: "Run fixture operation", Operation: &op}}})
	fixturekit.Must(t, err)
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
			f, _, repo := journey.NewApprovedFixture(t, "capacity")
			f.S.Cfg.Project.Capacity.GitOperations = limit
			var streams []config.WorkstreamID
			actions := []string{service.SealAction, service.BuildAction, service.LandAction, service.RebaseAction, service.DriftAction, delivery.PublishAction, lifecycle.PublishUpstreamAction}
			for _, action := range append(slices.Clone(actions), "model-fixture") {
				stream, err := config.NewWorkstreamID()
				fixturekit.Must(t, err)
				fixturekit.Must(t, repo.CreateWorkstreamOn(context.Background(), stream, config.WorkspacesGit, time.Now().UTC(), intake.OwnerActor))
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
				Concurrent: service.ConcurrentOperation, Class: service.TestOperationClass, Limit: journey.InspectForTest(f.S).GitOperationLimit(repo, nil),
				Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: effects, coreadapter.RunnerBoundary: fixturekit.CompletedEffect{}}})
			fixturekit.Must(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); releaseOnce.Do(func() { close(release) }); fixturekit.Must(t, controller.Wait()) }()
			fixturekit.Must(t, controller.Pass(ctx))
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
				fixturekit.Must(t, err)
				if len(ops[0].History) > 0 {
					claimed++
				}
			}
			if claimed != want {
				t.Fatalf("claimed %d Git operations, want %d", claimed, want)
			}
			model, err := repo.Operations(streams[len(actions)])
			fixturekit.Must(t, err)
			if !model[0].Acknowledged {
				t.Fatal("Git capacity blocked model work or the pass")
			}
			releaseOnce.Do(func() { close(release) })
			fixturekit.Must(t, controller.Wait())
			for range len(actions) {
				fixturekit.Must(t, controller.Pass(ctx))
				fixturekit.Must(t, controller.Wait())
			}
			for _, stream := range streams {
				ops, err := repo.Operations(stream)
				fixturekit.Must(t, err)
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
	f, a, repo := journey.NewApprovedFixture(t, "landing-before-drift")
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	b := journey.RecordBuild(t, f, repo, "independent-drift", intakefixture.IndependentPlan, config.WorkspacesGit, base)
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repo)}
	fixturekit.Must(t, lands.Pass(context.Background()))
	if len(journey.LandOperations(t, repo, a)) != 1 {
		t.Fatal("fixture has no pending landing")
	}
	requested, err := (journey.Drifter{Foreman: lands}).RequestDrifts(context.Background(), intakefixture.TestDrift)
	fixturekit.Must(t, err)
	if !slices.Equal(requested, []config.WorkstreamID{b}) {
		t.Fatalf("drift requests %v, want only independent workstream %s", requested, b)
	}
}

func TestGitCapacitySharesPollPermitsAndAdoptsLimits(t *testing.T) {
	t.Parallel()
	f, _, repo := journey.NewApprovedFixture(t, "shared-git-capacity")
	setLimit := func(limit int) {
		cfg := *f.S.Cfg
		cfg.Project.Capacity.GitOperations = limit
		f.S.Cfg = &cfg
		journey.InspectForTest(f.S).SetConfig(&cfg)
	}
	setLimit(2)
	poll, ok := journey.InspectForTest(f.S).TryGitOperation(repo)
	if !ok {
		t.Fatal("poll was not admitted")
	}
	defer poll()
	var otherUsed int
	other := func(coreadapter.Operation) (func(), bool) {
		otherUsed++
		return func() { otherUsed-- }, true
	}
	limit := journey.InspectForTest(f.S).GitOperationLimit(repo, other)
	op := coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: service.SealAction}
	workflow, ok := limit(op)
	if !ok {
		t.Fatal("workflow was not admitted alongside poll")
	}
	defer workflow()
	if _, ok := limit(op); ok || otherUsed != 1 {
		t.Fatalf("full pool admitted work or leaked another permit: used %d", otherUsed)
	}

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
	model, ok := limit(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: service.DraftAction})
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
	f, _, repo := journey.NewApprovedFixture(t, "busy-git-admission")
	stream, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstreamOn(context.Background(), stream, config.WorkspacesGit, f.S.Now(), intake.OwnerActor))
	queueGitFixtureOperation(t, repo, stream, service.SealAction, coreadapter.RepositoryBoundary, map[string]string{})
	queueGitFixtureOperation(t, repo, stream, service.BuildAction, coreadapter.RepositoryBoundary, map[string]string{})
	ops, err := repo.Operations(stream)
	fixturekit.Must(t, err)
	attempt, _, err := repo.BeginOperation(context.Background(), stream, ops[0].EventID, trace.WorkflowOperation, execution.ForemanActor, f.S.Now)
	fixturekit.Must(t, err)
	if attempt == nil {
		t.Fatal("fixture did not reserve its workstream")
	}
	defer attempt.Close()
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "busy-git-fixture",
		Workstreams: func() ([]config.WorkstreamID, error) { return []config.WorkstreamID{stream}, nil },
		Concurrent:  service.ConcurrentOperation, Class: service.TestOperationClass, Limit: journey.InspectForTest(f.S).GitOperationLimit(repo, nil),
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: fixturekit.CompletedEffect{}},
	})
	fixturekit.Must(t, err)
	defer func() { fixturekit.Must(t, controller.Wait()) }()

	repo.WakeWorkflow()
	fixturekit.Must(t, repo.WaitWorkflow(context.Background(), nil))
	fixturekit.Must(t, controller.Pass(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := repo.WaitWorkflow(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy admission woke another pass: %v", err)
	}

	release, admitted := journey.InspectForTest(f.S).TryGitOperation(repo)
	if !admitted {
		t.Fatal("busy workflow admission leaked a Git permit")
	}
	release()
	ctx, cancelWake := context.WithTimeout(context.Background(), fixturekit.DemoTimeout)
	defer cancelWake()
	fixturekit.Must(t, repo.WaitWorkflow(ctx, nil))
}

func TestGitWorkflowAdmissionHonorsPauseAndBaseWait(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "git-holds")
	f.S.Options.PullRequests = &fixturekit.FakePulls{}
	journey.InspectForTest(f.S).SetStoppedPullRequests(f.S.Options.PullRequests)
	store, _, err := runtime.Open(runtime.Inputs{Config: f.S.Cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{repo.Project(): {stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, store.Close()) })
	f.S.Store = store
	journey.InspectForTest(f.S).SetRuntimeStore(store)
	target := runtime.Target{Scope: "workstream", Project: repo.Project(), Workstream: stream}
	fixturekit.Must(t, f.S.Store.SetPause(runtime.Pause{Target: target, Mode: "soft", Source: runtime.PauseOwner, Reason: "owner pause", SetAt: f.S.Now()}))
	hold := journey.InspectForTest(f.S).Holding(repo)
	actions := []string{service.SealAction, service.BuildAction, service.LandAction, service.RebaseAction, service.DriftAction, delivery.PublishAction, lifecycle.PublishUpstreamAction}
	for _, action := range actions {
		want := slices.Contains([]string{service.LandAction, service.RebaseAction, delivery.PublishAction, lifecycle.PublishUpstreamAction}, action)
		if got := hold(stream, coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: action}); got != want {
			t.Fatalf("paused %s: held %t, want %t", action, got, want)
		}
	}
	fixturekit.Must(t, f.S.Store.ClearPause(target, "owner"))
	for _, action := range actions {
		if hold(stream, coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: action}) {
			t.Fatalf("resumed %s remains held", action)
		}
	}
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-base-wait", Revision: 1, Project: repo.Project(), Workstream: stream, At: f.S.Now(), Actor: execution.ForemanActor, Cause: "fixture"},
		Subject: lifecycle.BaseWaitSubject, To: "waiting", Reason: "the base is unavailable",
	}})
	fixturekit.Must(t, err)
	for _, action := range actions {
		if !hold(stream, coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: action}) {
			t.Fatalf("base wait admitted %s", action)
		}
	}
}
