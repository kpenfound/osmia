package thread

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

// testClock is a clock several turns read at once.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

// queueOn accepts turn id on a new thread of agent with role.
func queueOn(t *testing.T, r *trace.Repository, agent, role, id string) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agent, Project: project, Workstream: stream, At: timestamp, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message", Depth: 3}
	if err := r.CreateThread(context.Background(), trace.Agent{Header: h, Role: role, ThreadID: "thread_" + agent}); err != nil {
		t.Fatal(err)
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + agent + "_" + id, Project: project, Workstream: stream, At: timestamp, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "owner-message", Depth: 3}, AgentID: agent, ThreadID: "thread_" + agent, TurnID: id, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Message " + id}
	if _, err := r.EnqueueTurn(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

// dispatch publishes the turn operation of each agent's turn.
func dispatch(t *testing.T, r *trace.Repository, turns map[string]string) {
	t.Helper()
	var events []trace.Event
	for _, agent := range slices.Sorted(maps.Keys(turns)) {
		event := trace.EventID("dispatch", agent)
		op, err := TurnOperation(project, event, TurnInput{Workstream: stream, Agent: agent, Turn: turns[agent]})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, trace.Event{ID: event, Kind: "turn-dispatched", Body: "Dispatch " + agent, Operation: &op})
	}
	tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "dispatch", Revision: 1, Project: project, Workstream: stream, At: timestamp, Actor: trace.Actor{Kind: "service", ID: "scheduler"}, Cause: "owner-message", Depth: 3}, Subject: "dispatch", To: "dispatched", Reason: "Dispatched turns"}, Events: events}
	if _, err := r.Transact(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
}

// controller reconciles turn operations through a dispatcher over turns,
// each beside the pass.
func controller(t *testing.T, r *trace.Repository, clock *testClock, turns coreadapter.Turns) *reconcile.Controller {
	t.Helper()
	sessions := t.TempDir()
	d := Dispatcher{Runner: Runner{Store: r, Turns: turns, Now: clock.Now}, Prepare: func(_ context.Context, in TurnInput) (coreadapter.PreparedTurn, error) {
		return coreadapter.PreparedTurn{SessionDirectory: sessions + "/" + in.Agent + "/" + in.Turn}, nil
	}}
	c, err := reconcile.New(r, reconcile.Options{Worker: "test", Now: clock.Now, RetryDelay: time.Minute, Ticks: make(chan time.Time),
		Adapters:   map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: d},
		Concurrent: func(op coreadapter.Operation) bool { _, err := DecodeTurn(op); return err == nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// acknowledged returns the turn operations acknowledged with a result, by
// agent, and fails if one recorded more than one result.
func acknowledged(t *testing.T, r *trace.Repository) map[string]string {
	t.Helper()
	records, err := r.Operations(stream)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, record := range records {
		in, err := DecodeTurn(record.Operation)
		if err != nil {
			t.Fatal(err)
		}
		results := 0
		for _, a := range record.History {
			if a.Kind == "result" {
				results++
			}
		}
		if results > 1 {
			t.Fatalf("turn of %s recorded %d results", in.Agent, results)
		}
		if record.Acknowledged && record.Result != nil {
			out[in.Agent] = record.Result.Outcome
		}
	}
	return out
}

// Turns of two threads dispatched together run at the same time: each
// session waits until the other has started.
func TestTurnsOfTwoThreadsRunAtOnce(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	queueOn(t, repo, "other", "reviewer", "first")
	dispatch(t, repo, map[string]string{"agent": "first", "other": "first"})
	var mu sync.Mutex
	started, running, most := map[string]bool{}, 0, 0
	both := make(chan struct{})
	turns := fakeTurns(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		mu.Lock()
		started[p.Scope.Role] = true
		running++
		most = max(most, running)
		if len(started) == 2 {
			close(both)
		}
		mu.Unlock()
		defer func() { mu.Lock(); running--; mu.Unlock() }()
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			return coreadapter.SessionResult{}, fmt.Errorf("the %s session never overlapped the other", p.Scope.Role)
		}
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s-" + p.Scope.Role}, FinalResponse: "done"}, nil
	})
	c := controller(t, repo, &testClock{at: timestamp.Add(time.Second)}, turns)
	if err := c.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := acknowledged(t, repo); got["agent"] != "idle" || got["other"] != "idle" {
		t.Fatalf("turn results %v", got)
	}
	if most != 2 {
		t.Fatalf("at most %d sessions ran at once", most)
	}
}

// A restart after a turn was claimed without a captured result, or after its
// result was captured but not completed, completes the turn once without
// running its session again.
func TestInterruptedConcurrentTurnsCompleteOnce(t *testing.T) {
	for _, captured := range []bool{false, true} {
		t.Run(map[bool]string{false: "claimed", true: "captured"}[captured], func(t *testing.T) {
			ctx := context.Background()
			repo, root, p := setup(t)
			req := queue(t, repo, "first")
			dispatch(t, repo, map[string]string{"agent": "first"})
			clock := &testClock{at: timestamp.Add(time.Second)}
			at := clock.Now()
			if _, err := repo.ClaimTurn(ctx, stream, "agent", "token", "/owned", at); err != nil {
				t.Fatal(err)
			}
			a := trace.TurnAttempt{Number: 1, Profile: req.Profile, Path: "replay", Reason: "fresh", At: at}
			if err := repo.RecordAttempt(ctx, stream, "agent", "first", "token", a); err != nil {
				t.Fatal(err)
			}
			if captured {
				result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, SessionDirectory: "/owned", StartedAt: at, FinalResponse: "done", Usage: coreadapter.Usage{CostUSD: 2, CostKnown: true}}
				a.Result = &result
				if err := repo.RecordAttempt(ctx, stream, "agent", "first", "token", a); err != nil {
					t.Fatal(err)
				}
				h := req.Header
				h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), at, trace.Actor{Kind: "service", ID: "thread-runner"}
				if err := repo.CaptureTurn(ctx, "token", trace.TurnResponse{Header: h, AgentID: "agent", ThreadID: req.ThreadID, TurnID: "first", RequestID: req.ID, RequestRevision: 1, Result: result}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			repo, err := trace.Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			ran := 0
			turns := fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				ran++
				return coreadapter.SessionResult{}, errors.New("an interrupted turn ran again")
			})
			pass := func() {
				t.Helper()
				c := controller(t, repo, clock, turns)
				if err := c.Pass(ctx); err != nil {
					t.Fatal(err)
				}
				if err := c.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			want := "idle"
			if !captured {
				// A reservation may still be running, so the controller waits
				// until the service's startup recovery settles it.
				pass()
				if got := acknowledged(t, repo); len(got) != 0 {
					t.Fatalf("a reserved turn completed: %v", got)
				}
				if err := repo.AbandonTurn(ctx, stream, "agent", "first", clock.Now()); err != nil {
					t.Fatal(err)
				}
				clock.Advance(time.Minute)
				want = "interrupted"
			}
			pass()
			pass()
			if got := acknowledged(t, repo); !maps.Equal(got, map[string]string{"agent": want}) {
				t.Fatalf("turn results %v, want %s", got, want)
			}
			th, err := repo.Thread(stream, "agent")
			if err != nil || len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Response == nil || len(th.Turns[0].Attempts) != 1 {
				t.Fatalf("thread %+v %v", th, err)
			}
			if ran != 0 {
				t.Fatalf("the interrupted turn ran %d times", ran)
			}
			costs, err := trace.Read[trace.Cost](repo, stream)
			if wantCosts := map[bool]int{false: 0, true: 1}[captured]; err != nil || len(costs) != wantCosts {
				t.Fatalf("costs %+v %v, want %d", costs, err, wantCosts)
			}
		})
	}
}
