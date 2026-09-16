package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// blockingTurns delegates to scripted fake agent results, first calling the
// per-turn hook with the turn's context.
type blockingTurns struct {
	fake  *adaptertest.Turns
	mu    sync.Mutex
	hooks map[string]func(context.Context)
}

func (b *blockingTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	b.mu.Lock()
	hook := b.hooks[p.Scope.Turn]
	b.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return b.fake.Run(context.WithoutCancel(ctx), p)
}

// queueTurn accepts an owner message without publishing any intent to run it.
func queueTurn(t *testing.T, repo *trace.Repository, turn string, at time.Time) {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: stream, At: at, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message_" + turn, Depth: 1},
		AgentID: demoAgent, ThreadID: demoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	must(t, err)
}

func turnOperations(t *testing.T, repo *trace.Repository) map[string]trace.OperationRecord {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	byTurn := map[string]trace.OperationRecord{}
	for _, op := range ops {
		var in thread.TurnInput
		must(t, json.Unmarshal(op.Operation.Input, &in))
		byTurn[in.Turn] = op
	}
	return byTurn
}

func TestServiceRunsQueuedTurnsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sc-")
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
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	must(t, repo.Close())

	reply := func(turn string) adaptertest.Reply[coreadapter.SessionResult] {
		return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer " + turn}}
	}
	turns := &blockingTurns{fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply("first"), reply("second"))}, hooks: map[string]func(context.Context){}}
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	// Ticks are never sent in the first lifetime: nothing outside the service
	// triggers the turn.
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks

	entered := make(chan struct{})
	turns.hooks["first"] = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
	}
	s, err := Start(ctx, opts)
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		s.Close()
		t.Fatal("queued turn did not start")
	}
	bound.Lock()
	repo = live
	bound.Unlock()
	// A message queued mid-turn waits; the thread keeps a single turn in flight.
	queueTurn(t, repo, "second", clock.Now())
	th, err := repo.Thread(stream, demoAgent)
	must(t, err)
	if th.Active != "first" || len(th.Turns) != 2 || th.Turns[1].Claim != nil {
		t.Fatalf("mid-turn thread: %+v", th)
	}
	if ops := turnOperations(t, repo); len(ops) != 1 || ops["first"].Claim == nil {
		t.Fatalf("mid-turn operations: %+v", ops)
	}
	must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	th, err = repo.Thread(stream, demoAgent)
	must(t, err)
	if th.Active != "" || th.Turns[0].CompletedAt.IsZero() || th.Turns[1].Claim != nil {
		t.Fatalf("stopped thread: %+v", th)
	}
	ops := turnOperations(t, repo)
	if len(ops) != 1 || ops["first"].Result != nil || ops["first"].Acknowledged {
		t.Fatalf("stopped operations: %+v", ops)
	}
	must(t, repo.Close())

	// Second lifetime: the startup pass recovers the first turn and runs the second.
	s, err = Start(ctx, opts)
	must(t, err)
	select {
	case ticks <- clock.Now():
	case <-time.After(demoTimeout):
		s.Close()
		t.Fatal("restarted service did not finish its startup pass")
	}
	must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	calls := turns.fake.Calls()
	var ran []string
	for _, call := range calls {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first", "second"}) {
		t.Fatalf("backend runs %v", ran)
	}
	th, err = repo.Thread(stream, demoAgent)
	must(t, err)
	if th.Active != "" || th.Status != "idle" || len(th.Turns) != 2 {
		t.Fatalf("final thread: %+v", th)
	}
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() || len(q.Attempts) != 1 {
			t.Fatalf("turn %s: %+v", q.Request.TurnID, q)
		}
	}
	ops = turnOperations(t, repo)
	if len(ops) != 2 {
		t.Fatalf("operations %+v", ops)
	}
	for turn, op := range ops {
		if !op.Acknowledged || op.Result == nil || op.Result.Outcome != "idle" || op.Transition.Actor != scheduler.Actor || op.Transition.Cause != "request_"+turn {
			t.Fatalf("operation %s: %+v", turn, op)
		}
	}
	if op := ops["first"]; op.Observation == nil || op.Observation.State != coreadapter.EffectCompleted {
		t.Fatalf("first operation was not finished by inspection: %+v", op)
	}
}

func TestServicePauseHoldsWorkerTurnsUntilCleared(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sp-")
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
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply)}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	s, c := start(t, opts)
	repo = <-lives
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(demoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	ran := func() []string {
		var out []string
		for _, call := range turns.Calls() {
			out = append(out, call.Scope.Turn)
		}
		return out
	}

	target := runtime.Target{Scope: "workstream", Project: project, Workstream: stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "operator"})
	queueTurn(t, repo, "held", clock.Now())
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: chief"}
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	// A tick is taken only once the previous pass has finished.
	tick()
	tick()
	if got := ran(); !slices.Equal(got, []string{"chief"}) {
		t.Fatalf("paused runs %v", got)
	}
	if _, ok := turnOperations(t, repo)["held"]; ok {
		t.Fatal("paused worker turn was dispatched")
	}

	// Clearing the pause is enough: the periodic pass runs the held turn with
	// no new message or operation.
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
	tick()
	tick()
	if got := ran(); !slices.Equal(got, []string{"chief", "held"}) {
		t.Fatalf("resumed runs %v", got)
	}
	must(t, s.Close())
}

func TestServiceParksWaitingThreadAcrossRestart(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sw-")
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
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "ask", clock.Now())
	must(t, repo.Close())

	waiting := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Question", Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Owner answer needed"}}}
	answered := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Thanks"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](waiting, answered)}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(demoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	ran := func() []string {
		var out []string
		for _, call := range turns.Calls() {
			out = append(out, call.Scope.Turn)
		}
		return out
	}
	parked := func(r *trace.Repository) bool {
		t.Helper()
		th, err := r.Thread(stream, demoAgent)
		must(t, err)
		return th.Parked()
	}

	s, err := Start(ctx, opts)
	must(t, err)
	repo = <-lives
	tick()
	tick()
	if got := ran(); !slices.Equal(got, []string{"ask"}) || !parked(repo) {
		t.Fatalf("first lifetime ran %v, parked %v", got, parked(repo))
	}
	must(t, s.Close())

	s, err = Start(ctx, opts)
	must(t, err)
	repo = <-lives
	tick()
	tick()
	if got := ran(); !slices.Equal(got, []string{"ask"}) || !parked(repo) {
		t.Fatalf("after restart ran %v, parked %v", got, parked(repo))
	}

	queueTurn(t, repo, "answer", clock.Now())
	if parked(repo) {
		t.Fatal("queued turn left the thread parked")
	}
	tick()
	tick()
	if got := ran(); !slices.Equal(got, []string{"ask", "answer"}) || parked(repo) {
		t.Fatalf("unparked runs %v, parked %v", got, parked(repo))
	}
	must(t, s.Close())
}
