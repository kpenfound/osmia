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

const sibling config.WorkstreamID = "w_0123456789abcdef0123456789abcdee"

type turnFunc func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (f turnFunc) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f(ctx, p)
}

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
		{project: intakefixture.Project, stream: intakefixture.Stream, stoppable: true},
		{project: intakefixture.Project, stream: intakefixture.Stream},
		{project: intakefixture.Project, stream: sibling, stoppable: true},
		{project: other, stream: intakefixture.Stream, stoppable: true},
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
	untrack = append(untrack, r.add(intakefixture.Stream, func() { t.Error("a pause cancelled a turn") }))
	pause := func(mode, scope string, p config.ProjectID, w config.WorkstreamID) []runtime.Pause {
		return []runtime.Pause{{Target: runtime.Target{Scope: scope, Project: p, Workstream: w}, Mode: mode, Source: runtime.PauseOwner, Reason: scope + " reason"}}
	}
	for _, step := range []struct {
		pauses  []runtime.Pause
		stopped []int
	}{
		{pause("soft", "factory", "", ""), nil},
		{pause("hard", "workstream", intakefixture.Project, intakefixture.Stream), []int{0}},
		{pause("hard", "project", intakefixture.Project, ""), []int{0, 2}},
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
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{intakefixture.Stream}}})
	intakefixture.Must(t, err)
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
// force completes as stopped without calling the backend; a chief-of-staff
// turn runs through a factory-wide hard pause.
func TestHardPauseStopsATurnBeforeItStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, clock := hardPauseRepository(t, 1)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	_, err := repo.EnsureChiefOfStaff(ctx, intakefixture.Stream, clock.Now(), owner)
	intakefixture.Must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: pause everything, I'm travelling"}
	_, err = repo.EnqueueTurn(ctx, req)
	intakefixture.Must(t, err)
	backend := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "chief"}, FinalResponse: "Paused everything"}})}
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	intakefixture.Must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Travelling", SetAt: clock.Now()}))

	op, err := thread.TurnOperation(intakefixture.Project, "event-worker", thread.TurnInput{Workstream: intakefixture.Stream, Agent: "agent-0", Turn: "t-0"})
	intakefixture.Must(t, err)
	result, err := a.Apply(ctx, op)
	intakefixture.Must(t, err)
	if result.Outcome != "interrupted" || len(backend.Calls()) != 0 {
		t.Fatalf("result %+v, backend calls %d", result, len(backend.Calls()))
	}
	th, err := repo.Thread(intakefixture.Stream, "agent-0")
	intakefixture.Must(t, err)
	checkPauseStop(t, th.Turns[0], "factory", "Travelling")
	if len(th.Turns[0].Attempts) != 0 {
		t.Fatalf("a turn stopped before it started records attempts: %+v", th.Turns[0].Attempts)
	}

	op, err = thread.TurnOperation(intakefixture.Project, "event-chief", thread.TurnInput{Workstream: intakefixture.Stream, Agent: trace.ChiefOfStaff, Turn: "chief"})
	intakefixture.Must(t, err)
	result, err = a.Apply(ctx, op)
	intakefixture.Must(t, err)
	if result.Outcome != "idle" || len(backend.Calls()) != 1 {
		t.Fatalf("chief-of-staff turn under a factory hard pause: %+v, backend calls %d", result, len(backend.Calls()))
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
	backend := turnFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		started <- p.Scope.Thread
		result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-" + p.Scope.Thread}, FinalResponse: "Partial"}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(intakefixture.DemoTimeout):
			return result, errors.New("the turn ran on under a hard pause")
		}
	})
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			op, err := thread.TurnOperation(intakefixture.Project, fmt.Sprintf("event-%d", i), thread.TurnInput{Workstream: intakefixture.Stream, Agent: fmt.Sprintf("agent-%d", i), Turn: fmt.Sprintf("t-%d", i)})
			if err == nil {
				_, err = a.Apply(context.Background(), op)
			}
			errs <- err
		})
	}
	<-started
	intakefixture.Must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: intakefixture.Project, Workstream: intakefixture.Stream}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Hold everything", SetAt: clock.Now()}))
	wg.Wait()
	close(errs)
	for err := range errs {
		intakefixture.Must(t, err)
	}
	for i := range n {
		th, err := repo.Thread(intakefixture.Stream, fmt.Sprintf("agent-%d", i))
		intakefixture.Must(t, err)
		checkPauseStop(t, th.Turns[0], "workstream", "Hold everything")
	}
}

func TestHardPauseStopsCoveredRunningTurnsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "hp-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, clock.Now(), owner))
	intakefixture.Must(t, repo.CreateWorkstream(ctx, sibling, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: intakefixture.DemoAgent, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: intakefixture.DemoThread}
	intakefixture.Must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	intakefixture.Must(t, repo.Close())

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
		case <-time.After(intakefixture.DemoTimeout):
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
		case <-time.After(intakefixture.DemoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	completed := func(agent, turn string) trace.QueuedTurn {
		t.Helper()
		var done trace.QueuedTurn
		intakefixture.Soon(t, "turn "+turn+" to complete", func() bool {
			th, err := repo.Thread(intakefixture.Stream, agent)
			intakefixture.Must(t, err)
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

	target := runtime.Target{Scope: "workstream", Project: intakefixture.Project, Workstream: intakefixture.Stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: intakefixture.Project, Workstream: sibling}, Mode: "hard", Reason: "Another feature", Source: "owner"})
	running("a hard pause of another workstream stopped the turn")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Finish up", Source: "owner"})
	running("a soft pause stopped the turn")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop now", Source: "owner"})
	await(stopped, "the hard pause did not stop the running turn")
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
	close(settling)
	first := completed(intakefixture.DemoAgent, "first")
	checkPauseStop(t, first, "workstream", "Stop now")
	if first.Response.Result.FinalResponse != "Answer first" || first.Response.Result.Session.ID != "first" {
		t.Fatalf("the stopped turn lost its partial result: %+v", first.Response)
	}
	tick()
	tick()
	checkNoRetry(t, turnOperations(t, repo)["first"], "interrupted")

	factory := runtime.Target{Scope: "factory"}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: factory, Mode: "hard", Reason: "Travelling", Source: "owner"})
	chiefEntered, chiefRelease := make(chan struct{}), make(chan struct{})
	var chiefErr error
	turns.Hooks["chief"] = func(ctx context.Context) {
		close(chiefEntered)
		<-chiefRelease
		chiefErr = ctx.Err()
	}
	queueTurn(t, repo, "second", clock.Now())
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: how is it going?"}
	_, err = repo.EnqueueTurn(ctx, req)
	intakefixture.Must(t, err)
	await(chiefEntered, "the chief of staff did not answer during a factory hard pause")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: intakefixture.Project}, Mode: "hard", Reason: "Really stop", Source: "owner"})
	close(chiefRelease)
	chief := completed(trace.ChiefOfStaff, "chief")
	if chiefErr != nil || chief.Status() != "idle" || chief.Response.Stop != nil {
		t.Fatalf("chief-of-staff turn under a hard pause: %v %+v", chiefErr, chief)
	}
	tick()
	tick()
	if _, ok := turnOperations(t, repo)["second"]; ok {
		t.Fatal("a worker turn was dispatched under a hard pause")
	}

	mutation(t, c, "DELETE", "pause", ClearPauseRequest(factory))
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(runtime.Target{Scope: "project", Project: intakefixture.Project}))
	tick()
	tick()
	if second := completed(intakefixture.DemoAgent, "second"); second.Status() != "idle" {
		t.Fatalf("resumed turn %+v", second)
	}
	var ran []string
	for _, call := range turns.Fake.Calls() {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first", "chief", "second"}) {
		t.Fatalf("backend runs %v", ran)
	}
	intakefixture.Must(t, s.Close())
}
