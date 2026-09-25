package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// sibling is a second workstream of the demo project.
const sibling config.WorkstreamID = "w_0123456789abcdef0123456789abcdee"

// turnFunc is a fake agent backend.
type turnFunc func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (f turnFunc) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f(ctx, p)
}

// checkPauseStop checks that the turn completed interrupted by a hard pause
// of the scope, with no failure.
func checkPauseStop(t *testing.T, q trace.QueuedTurn, scope, reason string) {
	t.Helper()
	r := q.Response
	want := trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: scope, Source: runtime.PauseOwner, Reason: reason}
	if r == nil || q.CompletedAt.IsZero() || q.Status() != "interrupted" || r.Stop == nil || *r.Stop != want || r.Failure != "" || r.FailureClass != "" || r.Classification != nil {
		t.Fatalf("turn %s was not stopped by the %s pause: %+v", q.Request.TurnID, scope, q)
	}
}

// checkNoRetry checks that the turn's operation completed with the turn's
// status and was never scheduled for a retry.
func checkNoRetry(t *testing.T, op trace.OperationRecord, outcome string) {
	t.Helper()
	if !op.Acknowledged || op.Result == nil || op.Result.Outcome != outcome || !op.RetryAt.IsZero() || slices.ContainsFunc(op.History, func(a trace.OperationAction) bool { return a.Kind == "retry" }) {
		t.Fatalf("turn operation: %+v", op)
	}
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
		{project: project, stream: stream, stoppable: true},
		{project: project, stream: stream}, // a chief-of-staff turn
		{project: project, stream: sibling, stoppable: true},
		{project: other, stream: stream, stoppable: true},
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
	// An abandonable reconciler's own turn is never stopped.
	untrack = append(untrack, r.add(stream, func() { t.Error("a pause cancelled a turn") }))
	pause := func(mode, scope string, p config.ProjectID, w config.WorkstreamID) []runtime.Pause {
		return []runtime.Pause{{Target: runtime.Target{Scope: scope, Project: p, Workstream: w}, Mode: mode, Source: runtime.PauseOwner, Reason: scope + " reason"}}
	}
	for _, step := range []struct {
		pauses  []runtime.Pause
		stopped []int
	}{
		{pause("soft", "factory", "", ""), nil},
		{pause("hard", "workstream", project, stream), []int{0}},
		{pause("hard", "project", project, ""), []int{0, 2}},
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

// hardPauseRepository creates the demo project's trace with the workstream
// and n mason threads, agent-<i>, each with its turn t-<i> queued.
func hardPauseRepository(t *testing.T, n int) (*config.Config, *trace.Repository, *demoClock) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	cfg, err := config.Load(fixtureAt(t, home).Config)
	must(t, err)
	clock := &demoClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	for i := range n {
		agent := fmt.Sprintf("agent-%d", i)
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: masonRole, ThreadID: agent}
		must(t, repo.CreateThread(ctx, identity))
		turn := fmt.Sprintf("t-%d", i)
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "message_" + turn, Depth: 1},
			AgentID: agent, ThreadID: agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work " + turn}
		_, err := repo.EnqueueTurn(ctx, req)
		must(t, err)
	}
	return cfg, repo, clock
}

// hardPauseService is a service with only the runtime store and the running
// turns, and an abandonable reconciler running turns through backend.
func hardPauseService(t *testing.T, cfg *config.Config, repo *trace.Repository, clock *demoClock, backend coreadapter.Turns) (*Service, abandonable) {
	t.Helper()
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: []config.WorkstreamID{stream}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}, store: store}
	a := abandonable{Reconciler: thread.Dispatcher{Runner: thread.Runner{Store: repo, Turns: backend, Now: clock.Now},
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
	_, err := repo.EnsureChiefOfStaff(ctx, stream, clock.Now(), owner)
	must(t, err)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: pause everything, I'm travelling"}
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	backend := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "chief"}, FinalResponse: "Paused everything"}})}
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Travelling", SetAt: clock.Now()}))

	op, err := thread.TurnOperation(project, "event-worker", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	result, err := a.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "interrupted" || len(backend.Calls()) != 0 {
		t.Fatalf("result %+v, backend calls %d", result, len(backend.Calls()))
	}
	th, err := repo.Thread(stream, "agent-0")
	must(t, err)
	checkPauseStop(t, th.Turns[0], "factory", "Travelling")
	if len(th.Turns[0].Attempts) != 0 {
		t.Fatalf("a turn stopped before it started records attempts: %+v", th.Turns[0].Attempts)
	}

	op, err = thread.TurnOperation(project, "event-chief", thread.TurnInput{Workstream: stream, Agent: trace.ChiefOfStaff, Turn: "chief"})
	must(t, err)
	result, err = a.Apply(ctx, op)
	must(t, err)
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
		case <-time.After(demoTimeout):
			return result, errors.New("the turn ran on under a hard pause")
		}
	})
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			op, err := thread.TurnOperation(project, fmt.Sprintf("event-%d", i), thread.TurnInput{Workstream: stream, Agent: fmt.Sprintf("agent-%d", i), Turn: fmt.Sprintf("t-%d", i)})
			if err == nil {
				_, err = a.Apply(context.Background(), op)
			}
			errs <- err
		})
	}
	// Set the pause once some turns are running and others may not be.
	<-started
	must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "hard", Source: runtime.PauseOwner, Reason: "Hold everything", SetAt: clock.Now()}))
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	for i := range n {
		th, err := repo.Thread(stream, fmt.Sprintf("agent-%d", i))
		must(t, err)
		checkPauseStop(t, th.Turns[0], "workstream", "Hold everything")
	}
}

// A hard pause stops the running turn it covers and no other; clearing the
// pause while the stop settles keeps the stop, and the chief of staff answers
// the owner through a factory-wide hard pause.
func TestHardPauseStopsCoveredRunningTurnsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "hp-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	root := cfg.Root.String()

	clock := &demoClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	must(t, repo.CreateWorkstream(ctx, sibling, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	must(t, repo.Close())

	reply := func(id string, cancelled bool) adaptertest.Reply[coreadapter.SessionResult] {
		return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: id}, FinalResponse: "Answer " + id, Cancelled: cancelled}}
	}
	turns := &blockingTurns{fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply("first", true), reply("chief", false), reply("second", false))}, hooks: map[string]func(context.Context){}}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks

	entered, stopped, settling := make(chan struct{}), make(chan struct{}), make(chan struct{})
	turns.hooks["first"] = func(ctx context.Context) {
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
		case <-time.After(demoTimeout):
			t.Fatal(what)
		}
	}
	running := func(what string) {
		t.Helper()
		// Setting a pause stops what it covers before the request returns.
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
		case <-time.After(demoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	completed := func(agent, turn string) trace.QueuedTurn {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for {
			th, err := repo.Thread(stream, agent)
			must(t, err)
			if i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }); i >= 0 && !th.Turns[i].CompletedAt.IsZero() {
				return th.Turns[i]
			}
			if time.Now().After(deadline) {
				t.Fatalf("turn %s did not complete: %+v", turn, th)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	await(entered, "queued turn did not start")

	target := runtime.Target{Scope: "workstream", Project: project, Workstream: stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: sibling}, Mode: "hard", Reason: "Another feature", Source: "owner"})
	running("a hard pause of another workstream stopped the turn")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Finish up", Source: "owner"})
	running("a soft pause stopped the turn")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop now", Source: "owner"})
	await(stopped, "the hard pause did not stop the running turn")
	// Resume while the stop is settling.
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
	close(settling)
	first := completed(demoAgent, "first")
	checkPauseStop(t, first, "workstream", "Stop now")
	if first.Response.Result.FinalResponse != "Answer first" || first.Response.Result.Session.ID != "first" {
		t.Fatalf("the stopped turn lost its partial result: %+v", first.Response)
	}
	tick()
	tick()
	checkNoRetry(t, turnOperations(t, repo)["first"], "interrupted")

	// Everything is paused: the chief of staff still answers, and a hard pause
	// set during its own turn leaves that turn running.
	factory := runtime.Target{Scope: "factory"}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: factory, Mode: "hard", Reason: "Travelling", Source: "owner"})
	chiefEntered, chiefRelease := make(chan struct{}), make(chan struct{})
	var chiefErr error
	turns.hooks["chief"] = func(ctx context.Context) {
		close(chiefEntered)
		<-chiefRelease
		chiefErr = ctx.Err()
	}
	queueTurn(t, repo, "second", clock.Now())
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: how is it going?"}
	// Queuing wakes the loop, which blocks in the chief's turn until released.
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	await(chiefEntered, "the chief of staff did not answer during a factory hard pause")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: project}, Mode: "hard", Reason: "Really stop", Source: "owner"})
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

	// Resuming runs the thread's queued work.
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(factory))
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(runtime.Target{Scope: "project", Project: project}))
	tick()
	tick()
	if second := completed(demoAgent, "second"); second.Status() != "idle" {
		t.Fatalf("resumed turn %+v", second)
	}
	var ran []string
	for _, call := range turns.fake.Calls() {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first", "chief", "second"}) {
		t.Fatalf("backend runs %v", ran)
	}
	must(t, s.Close())
}

// A hard pause stops a unit's running mason turn with its view kept in the
// unit's workspace; resuming continues the unit on the same thread from the
// stopped session.
func TestHardPauseStopsAMasonAndResumeContinuesIt(t *testing.T) {
	t.Parallel()
	f, _ := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()
	const stoppedFile = "internal/trace/stopped.go"
	recoverTurn := masonAgent("resume") + "-recover-1"
	entered := make(chan struct{})
	var mu sync.Mutex
	var continuation *agent.Request
	var sawStoppedFile bool
	f.engine.mu.Lock()
	f.engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	f.engine.turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), stoppedFile), []byte("package trace\n"), 0644); err != nil {
			return nil, err
		}
		close(entered)
		<-ctx.Done()
		// The session a stop kills ends with a signal.
		return &agent.Result{ClaudeID: "session-stopped", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
	}
	f.engine.turns[recoverTurn] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		_, err := os.Stat(filepath.Join(req.Workspace.Directory(), stoppedFile))
		mu.Lock()
		continuation, sawStoppedFile = &req, err == nil
		mu.Unlock()
		return &agent.Result{ClaudeID: "session-stopped", ResultText: "Continued", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()

	stream, _ := f.builtAs(t, "hard-pause")
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the mason did not start")
	}
	target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	turn := func(i int) trace.QueuedTurn {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for {
			th, err := f.repository().Thread(stream, masonAgent("resume"))
			must(t, err)
			if len(th.Turns) > i && !th.Turns[i].CompletedAt.IsZero() {
				return th.Turns[i]
			}
			if time.Now().After(deadline) {
				t.Fatalf("mason turn %d did not complete: %+v", i, th)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	stopped := turn(0)
	checkPauseStop(t, stopped, "workstream", "Stop the mason")
	settle()
	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
	if _, err := os.Stat(filepath.Join(workspace, stoppedFile)); err != nil {
		t.Fatalf("the stopped turn's view was not kept: %v", err)
	}
	state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitImplementing {
		t.Fatalf("stopped unit is %s", state.Value)
	}
	ops, err := f.repository().Operations(stream)
	must(t, err)
	for _, op := range ops {
		in, err := thread.DecodeTurn(op.Operation)
		if err != nil || in.Agent != masonAgent("resume") {
			continue
		}
		switch in.Turn {
		case masonTurnID("resume"):
			checkNoRetry(t, op, "interrupted")
		case recoverTurn:
			t.Fatal("the continuation was dispatched under the hard pause")
		}
	}
	f.engine.mu.Lock()
	ran := slices.Contains(f.engine.runs, recoverTurn)
	f.engine.mu.Unlock()
	if ran {
		t.Fatal("the continuation ran under the hard pause")
	}

	mutation(t, f.c, "DELETE", "pause", target)
	next := turn(1)
	if next.Request.TurnID != recoverTurn || next.Request.ThreadID != stopped.Request.ThreadID || !strings.Contains(next.Request.Prompt, "A hard pause stopped your last turn.") || !strings.HasPrefix(next.Request.Prompt, stopped.Request.Prompt) {
		t.Fatalf("continuation request %+v", next.Request)
	}
	if len(next.Attempts) != 1 || next.Attempts[0].Path != "resume" || next.Attempts[0].SourceSession.ID != "session-stopped" || next.Attempts[0].SourceSequence != stopped.Sequence {
		t.Fatalf("the continuation did not resume the stopped session: %+v", next.Attempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if continuation == nil || !sawStoppedFile || continuation.ResumeID != "session-stopped" {
		t.Fatalf("continuation %+v saw the stopped file %v", continuation, sawStoppedFile)
	}
}
