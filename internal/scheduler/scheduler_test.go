package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const project config.ProjectID = "p_00000000000000000000000000000001"
const stream config.WorkstreamID = "w_00000000000000000000000000000001"

var (
	start = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	owner = trace.Actor{Kind: "owner", ID: "local"}
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

type fixture struct {
	root    config.Root
	project config.Project
	clock   *clock
}

func setup(t *testing.T, agents ...string) (*fixture, *trace.Repository) {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	f := &fixture{root: root, project: config.Project{ID: project, Clone: filepath.Join(base, "target")}, clock: &clock{now: start}}
	must(t, os.Mkdir(f.project.Clone, 0700))
	ctx := context.Background()
	repo, err := trace.Create(ctx, root, f.project, f.clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, f.clock.Now(), owner))
	for _, id := range agents {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: id, Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: "mason", ThreadID: "thread_" + id}))
	}
	return f, repo
}

func (f *fixture) reopen(t *testing.T, repo *trace.Repository) *trace.Repository {
	t.Helper()
	must(t, repo.Close())
	repo, err := trace.Open(f.root, f.project)
	must(t, err)
	return repo
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) queue(t *testing.T, repo *trace.Repository, agent, turn string) trace.TurnRequest {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + agent + "_" + turn, Project: project, Workstream: stream, Unit: "unit", At: f.clock.Now(), Actor: owner, Cause: "message_" + turn, Depth: 2},
		AgentID: agent, ThreadID: "thread_" + agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Message " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	must(t, err)
	return req
}

// dispatched lists the turn operations as agent/turn pairs in event order.
func dispatched(t *testing.T, repo *trace.Repository) []string {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	var got []string
	for _, op := range ops {
		if in, err := thread.DecodeTurn(op.Operation); err == nil {
			got = append(got, in.Agent+"/"+in.Turn)
		}
	}
	slices.Sort(got)
	return got
}

func result(turn string) adaptertest.Reply[coreadapter.SessionResult] {
	return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer " + turn}}
}

type turnsFunc func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (f turnsFunc) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f(ctx, p)
}

// controller binds the scheduler and a thread dispatcher to one reconciliation
// controller, as the service does.
func (f *fixture) controller(t *testing.T, repo *trace.Repository, turns coreadapter.Turns, admit func(context.Context, Candidate) (bool, error)) *reconcile.Controller {
	t.Helper()
	s, err := New(repo, Options{Now: f.clock.Now, Admit: admit})
	must(t, err)
	sessions := t.TempDir()
	d := thread.Dispatcher{Runner: thread.Runner{Store: repo, Turns: turns, Now: f.clock.Now},
		Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(sessions, in.Agent, in.Turn)}, nil
		}}
	c, err := reconcile.New(repo, reconcile.Options{Worker: "test", Now: f.clock.Now, RetryDelay: time.Hour, Ticks: make(chan time.Time),
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: d}, Schedule: s.Pass})
	must(t, err)
	return c
}

func TestPassDispatchesEachThreadsNextTurnOnce(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha", "beta", "idle")
	defer func() { repo.Close() }()
	first := f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "alpha", "two")
	f.queue(t, repo, "beta", "one")
	s, err := New(repo, Options{Now: f.clock.Now})
	must(t, err)
	must(t, s.Pass(ctx))
	if got, want := dispatched(t, repo), []string{"alpha/one", "beta/one"}; !slices.Equal(got, want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}
	// Level-triggered: another pass, also after reopening, publishes nothing new.
	must(t, s.Pass(ctx))
	repo = f.reopen(t, repo)
	s, err = New(repo, Options{Now: f.clock.Now})
	must(t, err)
	must(t, s.Pass(ctx))
	ops, err := repo.Operations(stream)
	must(t, err)
	if len(ops) != 2 {
		t.Fatalf("operations after repeated passes: %d", len(ops))
	}
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	if len(transitions) != 2 {
		t.Fatalf("transitions %d", len(transitions))
	}
	var tr trace.Transition
	for _, candidate := range transitions {
		if candidate.Cause == first.ID {
			tr = candidate
		}
	}
	if tr.Subject != Subject("alpha") || tr.From != "" || tr.To != "one" || tr.Actor != Actor || tr.Depth != first.Depth || tr.Unit != first.Unit || tr.Workstream != stream {
		t.Fatalf("dispatch transition: %+v", tr)
	}
	state, err := repo.Workflow(stream, Subject("alpha"))
	must(t, err)
	if state != (trace.WorkflowState{Version: 1, Value: "one"}) {
		t.Fatalf("alpha dispatch state %+v", state)
	}
	if Subject("alpha") == Subject("beta") || len(Subject(string(make([]byte, 128)))) != 49 {
		t.Fatal("subjects are not distinct and bounded")
	}
}

func TestPassLeavesThreadsWithATurnInFlight(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "claimed", "operated")
	defer repo.Close()
	// A claim without an operation, as a direct runner call leaves it.
	f.queue(t, repo, "claimed", "one")
	f.queue(t, repo, "claimed", "two")
	_, err := repo.ClaimTurn(ctx, stream, "claimed", "token", t.TempDir(), f.clock.Now())
	must(t, err)
	// An unfinished turn with an operation published by someone else.
	f.queue(t, repo, "operated", "one")
	f.queue(t, repo, "operated", "two")
	op, err := thread.TurnOperation(project, "event_external", thread.TurnInput{Workstream: stream, Agent: "operated", Turn: "one"})
	must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "external", Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "test"}, Subject: "inbox", To: "delivering", Reason: "External intent"},
		Events: []trace.Event{{ID: "event_external", Kind: "turn", Body: "Deliver one", Operation: &op}}})
	must(t, err)
	s, err := New(repo, Options{Now: f.clock.Now})
	must(t, err)
	must(t, s.Pass(ctx))
	if got, want := dispatched(t, repo), []string{"operated/one"}; !slices.Equal(got, want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}
}

// publish stores a turn operation with raw input, as any trace writer may.
func (f *fixture) publish(t *testing.T, repo *trace.Repository, name, input string) {
	t.Helper()
	event := trace.EventID(name, "turn")
	op := coreadapter.Operation{ID: trace.OperationID(project, stream, event), Boundary: coreadapter.RunnerBoundary, Action: thread.TurnAction, Input: json.RawMessage(input)}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: name, Project: project, Workstream: stream, At: f.clock.Now(), Actor: owner, Cause: "test"}, Subject: name, To: "delivering", Reason: "Raw intent"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Deliver", Operation: &op}}})
	must(t, err)
}

func TestUndecodableTurnOperationsDoNotStopDispatch(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha", "beta")
	defer repo.Close()
	f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "beta", "one")
	f.publish(t, repo, "array", `[]`)
	f.publish(t, repo, "typed", `{"agent":1}`)
	// The dispatcher refuses unknown fields, so this does not dispatch alpha/one.
	f.publish(t, repo, "extra", `{"workstream":"`+string(stream)+`","agent":"alpha","turn":"one","extra":true}`)
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](result("one"), result("one"))}
	c := f.controller(t, repo, turns, nil)
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if got, want := dispatched(t, repo), []string{"alpha/one", "beta/one"}; !slices.Equal(got, want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}
	if calls := turns.Calls(); len(calls) != 2 {
		t.Fatalf("turns run %+v", calls)
	}
	ops, err := repo.Operations(stream)
	must(t, err)
	retried := 0
	for _, op := range ops {
		if _, err := thread.DecodeTurn(op.Operation); err != nil {
			if op.Acknowledged || op.RetryAt.IsZero() {
				t.Fatalf("undecodable operation %+v", op)
			}
			retried++
		} else if !op.Acknowledged {
			t.Fatalf("turn operation %+v", op)
		}
	}
	if retried != 3 {
		t.Fatalf("retried operations %d", retried)
	}
}

func TestAdmitGatesDispatch(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha", "beta")
	defer repo.Close()
	req := f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "beta", "one")
	var offered []Candidate
	s, err := New(repo, Options{Now: f.clock.Now, Admit: func(_ context.Context, c Candidate) (bool, error) {
		offered = append(offered, c)
		return c.Thread.Identity.ID == "alpha", nil
	}})
	must(t, err)
	must(t, s.Pass(ctx))
	if got, want := dispatched(t, repo), []string{"alpha/one"}; !slices.Equal(got, want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}
	if len(offered) != 2 || offered[0].Workstream != stream || offered[0].Turn.Request.ID != req.ID || offered[1].Thread.Identity.ID != "beta" {
		t.Fatalf("candidates %+v", offered)
	}
	// A declined candidate is offered again; an admitted one is not re-offered.
	offered = nil
	must(t, s.Pass(ctx))
	if len(offered) != 1 || offered[0].Thread.Identity.ID != "beta" {
		t.Fatalf("second pass candidates %+v", offered)
	}
	failure := errors.New("gate unavailable")
	s.options.Admit = func(context.Context, Candidate) (bool, error) { return false, failure }
	if err := s.Pass(ctx); !errors.Is(err, failure) {
		t.Fatalf("gate failure: %v", err)
	}
}

func TestControllerRunsQueuedTurnsInOrder(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha")
	defer repo.Close()
	f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "alpha", "two")
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](result("one"), result("two"))}
	c := f.controller(t, repo, turns, nil)
	// Each pass dispatches and runs the next turn; a thread runs turns one at a time.
	must(t, c.Pass(ctx))
	if calls := turns.Calls(); len(calls) != 1 || calls[0].Scope.Turn != "one" {
		t.Fatalf("first pass calls %+v", calls)
	}
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	calls := turns.Calls()
	if len(calls) != 2 || calls[1].Scope.Turn != "two" || calls[1].Prompt != "Message two" {
		t.Fatalf("calls %+v", calls)
	}
	th, err := repo.Thread(stream, "alpha")
	must(t, err)
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() || q.Status() != "idle" || len(q.Attempts) != 1 {
			t.Fatalf("turn %s: %+v", q.Request.TurnID, q)
		}
	}
	ops, err := repo.Operations(stream)
	must(t, err)
	for _, op := range ops {
		if !op.Acknowledged || op.Result == nil || op.Result.Outcome != "idle" {
			t.Fatalf("operation %+v", op)
		}
	}
}

func TestTurnQueuedMidTurnRunsNext(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha")
	defer repo.Close()
	f.queue(t, repo, "alpha", "one")
	s, err := New(repo, Options{Now: f.clock.Now})
	must(t, err)
	var ran []string
	turns := turnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		ran = append(ran, p.Scope.Turn)
		if p.Scope.Turn == "one" {
			f.queue(t, repo, "alpha", "two")
			if err := s.Pass(ctx); err != nil {
				t.Errorf("mid-turn pass: %v", err)
			}
			if got := dispatched(t, repo); !slices.Equal(got, []string{"alpha/one"}) {
				t.Errorf("dispatched mid-turn: %v", got)
			}
		}
		return result(p.Scope.Turn).Value, nil
	})
	c := f.controller(t, repo, turns, nil)
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if !slices.Equal(ran, []string{"one", "two"}) {
		t.Fatalf("ran %v", ran)
	}
	th, err := repo.Thread(stream, "alpha")
	must(t, err)
	if th.Active != "" || len(th.Turns) != 2 || th.Turns[1].CompletedAt.IsZero() {
		t.Fatalf("thread %+v", th)
	}
}

func TestRestartFinishesAnInFlightTurnWithoutRunningItAgain(t *testing.T) {
	f, repo := setup(t, "alpha")
	f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "alpha", "two")
	// The service stops while the first turn's backend is returning.
	ctx, cancel := context.WithCancel(context.Background())
	var ran []string
	turns := turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		ran = append(ran, p.Scope.Turn)
		cancel()
		return result(p.Scope.Turn).Value, nil
	})
	if err := f.controller(t, repo, turns, nil).Pass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped pass: %v", err)
	}
	ops, err := repo.Operations(stream)
	must(t, err)
	if len(ops) != 1 || ops[0].Result != nil || ops[0].Acknowledged {
		t.Fatalf("operation before restart: %+v", ops)
	}
	repo = f.reopen(t, repo)
	defer repo.Close()
	ctx = context.Background()
	c := f.controller(t, repo, turns, nil)
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if !slices.Equal(ran, []string{"one", "two"}) {
		t.Fatalf("ran %v", ran)
	}
	if got := dispatched(t, repo); !slices.Equal(got, []string{"alpha/one", "alpha/two"}) {
		t.Fatalf("dispatched %v", got)
	}
	ops, err = repo.Operations(stream)
	must(t, err)
	for _, op := range ops {
		if !op.Acknowledged {
			t.Fatalf("operation %+v", op)
		}
	}
	th, err := repo.Thread(stream, "alpha")
	must(t, err)
	if th.Turns[0].CompletedAt.IsZero() || th.Turns[1].CompletedAt.IsZero() || len(th.Turns[0].Attempts) != 1 {
		t.Fatalf("thread %+v", th)
	}
}

func TestRestartKeepsAnInterruptedTurnReserved(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "alpha")
	f.queue(t, repo, "alpha", "one")
	f.queue(t, repo, "alpha", "two")
	s, err := New(repo, Options{Now: f.clock.Now})
	must(t, err)
	must(t, s.Pass(ctx))
	// The process died after the runner claimed the turn, before any result.
	_, err = repo.ClaimTurn(ctx, stream, "alpha", "token", t.TempDir(), f.clock.Now())
	must(t, err)
	repo = f.reopen(t, repo)
	defer repo.Close()
	turns := &adaptertest.Turns{}
	c := f.controller(t, repo, turns, nil)
	must(t, c.Pass(ctx))
	must(t, c.Pass(ctx))
	if calls := turns.Calls(); len(calls) != 0 {
		t.Fatalf("interrupted turn ran again: %+v", calls)
	}
	if got := dispatched(t, repo); !slices.Equal(got, []string{"alpha/one"}) {
		t.Fatalf("dispatched %v", got)
	}
	th, err := repo.Thread(stream, "alpha")
	must(t, err)
	if th.Status != "interrupted" || th.Active != "one" || th.Turns[1].Claim != nil {
		t.Fatalf("thread %+v", th)
	}
	ops, err := repo.Operations(stream)
	must(t, err)
	if ops[0].Acknowledged || ops[0].RetryAt.IsZero() {
		t.Fatalf("interrupted operation %+v", ops[0])
	}
}

func TestNewRequiresRepository(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil repository accepted")
	}
}
