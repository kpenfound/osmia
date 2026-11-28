package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func checkPauseStop(t *testing.T, q trace.QueuedTurn, scope, reason string) {
	t.Helper()
	intakefixture.CheckPauseStop(t, q, scope, reason)
}

func checkNoRetry(t *testing.T, op trace.OperationRecord, outcome string) {
	t.Helper()
	intakefixture.CheckNoRetry(t, op, outcome)
}

func TestRunningTurnsStopWhatAHardPauseCovers(t *testing.T) {
	t.Parallel()
	const other config.ProjectID = "p_0123456789abcdef0123456789abcdee"
	type running struct {
		project   config.ProjectID
		stream    config.WorkstreamID
		stoppable bool
		stops     []*thread.Stop
		cancelled bool
	}
	turns := []*running{
		{project: fixturekit.Project, stream: fixturekit.Stream, stoppable: true},
		{project: fixturekit.Project, stream: fixturekit.Stream},
		{project: fixturekit.Project, stream: fixturekit.Sibling, stoppable: true},
		{project: other, stream: fixturekit.Stream, stoppable: true},
	}
	var r runningTurns
	var untrack []func()
	for _, turn := range turns {
		held := runningTurn{project: turn.project, cancel: func() { turn.cancelled = true }}
		if turn.stoppable {
			held.stop = func(s *thread.Stop) { turn.stops = append(turn.stops, s) }
		}
		untrack = append(untrack, r.track(turn.stream, held))
	}
	untrack = append(untrack, r.add(fixturekit.Stream, func() { t.Error("a pause cancelled a turn") }))
	pause := func(mode, scope string, p config.ProjectID, w config.WorkstreamID) []runtime.Pause {
		return []runtime.Pause{{Target: runtime.Target{Scope: scope, Project: p, Workstream: w}, Mode: mode, Source: runtime.PauseOwner, Reason: scope + " reason"}}
	}
	for _, step := range []struct {
		pauses  []runtime.Pause
		stopped []int
	}{
		{pause("soft", "factory", "", ""), nil},
		{pause("hard", "workstream", fixturekit.Project, fixturekit.Stream), []int{0}},
		{pause("hard", "project", fixturekit.Project, ""), []int{0, 2}},
		{pause("hard", "factory", "", ""), []int{0, 2, 3}},
	} {
		for _, turn := range turns {
			turn.stops = nil
		}
		if n := r.stop(step.pauses); n != len(step.stopped) {
			t.Fatalf("%+v stopped %d turns, want %v", step.pauses, n, step.stopped)
		}
		for i, turn := range turns {
			want := slices.Contains(step.stopped, i)
			if (len(turn.stops) == 1) != want || turn.cancelled {
				t.Fatalf("%+v: turn %d stops %+v cancelled %v, want stopped %v", step.pauses, i, turn.stops, turn.cancelled, want)
			}
			if want && turn.stops[0].TurnStop != (trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: step.pauses[0].Target.Scope, Source: runtime.PauseOwner, Reason: step.pauses[0].Reason}) {
				t.Fatalf("stop %+v", turn.stops[0])
			}
		}
	}
	for _, done := range untrack {
		done()
	}
	if n := r.stop(pause("hard", "factory", "", "")); n != 0 {
		t.Fatalf("finished turns stopped: %d", n)
	}
}

func hardPauseRepository(t *testing.T, n int) (*config.Config, *trace.Repository, *fixturekit.DemoClock) {
	t.Helper()
	return fixturekit.HardPauseRepository(t, n)
}

func hardPauseService(t *testing.T, cfg *config.Config, repo *trace.Repository, clock *fixturekit.DemoClock, backend coreadapter.Turns) (*Service, abandonable) {
	t.Helper()
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}, store: store, jev: &jev.Judge{}}
	s.router = s.newRouter()
	a := abandonable{Reconciler: thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: repo, Turns: backend, Now: clock.Now},
		Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)}, nil
		}}, s: s, repository: repo}
	return s, a
}

// A turn operation dispatched before a hard pause and applied while it is in
// force completes as stopped without calling the backend; a manager
// turn runs through a factory-wide hard pause.
func TestHardPauseStopsATurnBeforeItStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, clock := hardPauseRepository(t, 1)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	_, err := repo.EnsureManager(ctx, fixturekit.Stream, clock.Now(), owner)
	fixturekit.Must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: pause everything, I'm travelling"}
	_, err = repo.EnqueueTurn(ctx, req)
	fixturekit.Must(t, err)
	backend := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "chief"}, FinalResponse: "Paused everything"}})}
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	fixturekit.Must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Travelling", SetAt: clock.Now()}))

	op, err := thread.TurnOperation(fixturekit.Project, "event-worker", thread.TurnInput{Workstream: fixturekit.Stream, Agent: "agent-0", Turn: "t-0"})
	fixturekit.Must(t, err)
	result, err := a.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "interrupted" || len(backend.Calls()) != 0 {
		t.Fatalf("result %+v, backend calls %d", result, len(backend.Calls()))
	}
	th, err := repo.Thread(fixturekit.Stream, "agent-0")
	fixturekit.Must(t, err)
	checkPauseStop(t, th.Turns[0], "factory", "Travelling")
	if len(th.Turns[0].Attempts) != 0 {
		t.Fatalf("a turn stopped before it started records attempts: %+v", th.Turns[0].Attempts)
	}

	op, err = thread.TurnOperation(fixturekit.Project, "event-chief", thread.TurnInput{Workstream: fixturekit.Stream, Agent: trace.Manager, Turn: "chief"})
	fixturekit.Must(t, err)
	result, err = a.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "idle" || len(backend.Calls()) != 1 {
		t.Fatalf("manager turn under a factory hard pause: %+v, backend calls %d", result, len(backend.Calls()))
	}
	s.turns.mu.Lock()
	defer s.turns.mu.Unlock()
	for stream, held := range s.turns.streams {
		if len(held) != 0 {
			t.Fatalf("finished turns still held in %s: %d", stream, len(held))
		}
	}
}

// Turns starting while a hard pause is being set are each stopped, before or
// after their session starts; none runs on under the pause.
func TestHardPauseRacingTurnStartsStopsEveryTurn(t *testing.T) {
	t.Parallel()
	const n = 8
	cfg, repo, clock := hardPauseRepository(t, n)
	started := make(chan string, n)
	backend := fixturekit.TurnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		started <- p.Scope.Thread
		result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-" + p.Scope.Thread}, FinalResponse: "Partial"}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(fixturekit.DemoTimeout):
			return result, errors.New("the turn ran on under a hard pause")
		}
	})
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			op, err := thread.TurnOperation(fixturekit.Project, fmt.Sprintf("event-%d", i), thread.TurnInput{Workstream: fixturekit.Stream, Agent: fmt.Sprintf("agent-%d", i), Turn: fmt.Sprintf("t-%d", i)})
			if err == nil {
				_, err = a.Apply(context.Background(), op)
			}
			errs <- err
		})
	}
	<-started
	fixturekit.Must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Hold everything", SetAt: clock.Now()}))
	wg.Wait()
	close(errs)
	for err := range errs {
		fixturekit.Must(t, err)
	}
	for i := range n {
		th, err := repo.Thread(fixturekit.Stream, fmt.Sprintf("agent-%d", i))
		fixturekit.Must(t, err)
		checkPauseStop(t, th.Turns[0], "workstream", "Hold everything")
	}
}

func TestHardPauseStopsCoveredRunningTurnsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "hp-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Sibling, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	fixturekit.Must(t, repo.Close())

	reply := func(id string, cancelled bool) adaptertest.Reply[coreadapter.SessionResult] {
		return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: id}, FinalResponse: "Answer " + id, Cancelled: cancelled}}
	}
	turns := &fixturekit.BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply("first", true), reply("chief", false), reply("second", false))}, Hooks: map[string]func(context.Context){}}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks

	entered, stopped, settling := make(chan struct{}), make(chan struct{}), make(chan struct{})
	turns.Hooks["first"] = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		<-settling
	}
	s, c := start(t, opts)
	repo = <-lives
	await := func(ch chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal(what)
		}
	}
	running := func(what string) {
		t.Helper()
		select {
		case <-stopped:
			t.Fatal(what)
		default:
		}
	}
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	completed := func(agent, turn string) trace.QueuedTurn {
		t.Helper()
		var done trace.QueuedTurn
		intakefixture.Soon(t, "turn "+turn+" to complete", func() bool {
			th, err := repo.Thread(fixturekit.Stream, agent)
			fixturekit.Must(t, err)
			i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
			if i < 0 || th.Turns[i].CompletedAt.IsZero() {
				return false
			}
			done = th.Turns[i]
			return true
		})
		return done
	}
	await(entered, "queued turn did not start")

	target := runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Sibling}, Mode: "hard", Reason: "Another feature", Source: "owner"})
	running("a hard pause of another workstream stopped the turn")
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Finish up", Source: "owner"})
	running("a soft pause stopped the turn")
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop now", Source: "owner"})
	await(stopped, "the hard pause did not stop the running turn")
	intakefixture.Mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
	close(settling)
	first := completed(fixturekit.DemoAgent, "first")
	checkPauseStop(t, first, "workstream", "Stop now")
	if first.Response.Result.FinalResponse != "Answer first" || first.Response.Result.Session.ID != "first" {
		t.Fatalf("the stopped turn lost its partial result: %+v", first.Response)
	}
	tick()
	tick()
	checkNoRetry(t, turnOperations(t, repo)["first"], "interrupted")

	factory := runtime.Target{Scope: "factory"}
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: factory, Mode: "hard", Reason: "Travelling", Source: "owner"})
	managerEntered, managerRelease := make(chan struct{}), make(chan struct{})
	var managerErr error
	turns.Hooks["chief"] = func(ctx context.Context) {
		close(managerEntered)
		<-managerRelease
		managerErr = ctx.Err()
	}
	queueTurn(t, repo, "second", clock.Now())
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: how is it going?"}
	_, err = repo.EnqueueTurn(ctx, req)
	fixturekit.Must(t, err)
	await(managerEntered, "the manager did not answer during a factory hard pause")
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: fixturekit.Project}, Mode: "hard", Reason: "Really stop", Source: "owner"})
	close(managerRelease)
	manager := completed(trace.Manager, "chief")
	if managerErr != nil || manager.Status() != "idle" || manager.Response.Stop != nil {
		t.Fatalf("manager turn under a hard pause: %v %+v", managerErr, manager)
	}
	tick()
	tick()
	if _, ok := turnOperations(t, repo)["second"]; ok {
		t.Fatal("a worker turn was dispatched under a hard pause")
	}

	intakefixture.Mutation(t, c, "DELETE", "pause", ClearPauseRequest(factory))
	intakefixture.Mutation(t, c, "DELETE", "pause", ClearPauseRequest(runtime.Target{Scope: "project", Project: fixturekit.Project}))
	tick()
	tick()
	if second := completed(fixturekit.DemoAgent, "second"); second.Status() != "idle" {
		t.Fatalf("resumed turn %+v", second)
	}
	var ran []string
	for _, call := range turns.Fake.Calls() {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first", "chief", "second"}) {
		t.Fatalf("backend runs %v", ran)
	}
	fixturekit.Must(t, s.Close())
}
