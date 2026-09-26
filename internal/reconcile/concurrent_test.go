package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// overlapTimeout bounds how long a blocked fake effect waits for the others.
const overlapTimeout = 10 * time.Second

// sessions is a runner-boundary adapter whose effects block: each waits until
// every operation named in overlap has started, or until release is closed.
type sessions struct {
	mu               sync.Mutex
	started, applied []string
	inspections      []string
	overlap          []string
	all, release     chan struct{}
	returned         map[string]bool
	effects          map[string]coreadapter.OperationResult
}

func newSessions(overlap ...string) *sessions {
	return &sessions{overlap: overlap, all: make(chan struct{}), release: make(chan struct{}), returned: map[string]bool{}, effects: map[string]coreadapter.OperationResult{}}
}

func (s *sessions) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspections = append(s.inspections, op.Action)
	if result, ok := s.effects[op.Action]; ok {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "session finished", Result: &result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no session"}, nil
}

func (s *sessions) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	s.mu.Lock()
	s.started = append(s.started, op.Action)
	if len(s.overlap) > 0 && !slices.ContainsFunc(s.overlap, func(a string) bool { return !slices.Contains(s.started, a) }) {
		close(s.all)
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.returned[op.Action] = true; s.mu.Unlock() }()
	if slices.Contains(s.overlap, op.Action) {
		select {
		case <-s.all:
		case <-ctx.Done():
			return coreadapter.OperationResult{}, ctx.Err()
		case <-time.After(overlapTimeout):
			return coreadapter.OperationResult{}, fmt.Errorf("session %s never overlapped the others", op.Action)
		}
	} else if op.Action != "landing" {
		select {
		case <-s.release:
		case <-ctx.Done():
			// A cancelled session takes a moment to stop.
			time.Sleep(200 * time.Millisecond)
			return coreadapter.OperationResult{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, op.Action)
	result := coreadapter.OperationResult{Outcome: "idle", Evidence: "session " + op.Action + " finished"}
	s.effects[op.Action] = result
	return result, nil
}

func (s *sessions) snapshot() (inspections, started, applied []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.inspections), slices.Clone(s.started), slices.Clone(s.applied)
}

// publish adds one runner-boundary operation per action to the fixture's
// workstream.
func (f *fixture) publish(t *testing.T, actions ...string) {
	t.Helper()
	var events []trace.Event
	id := "sessions-" + actions[0]
	for _, action := range actions {
		event := trace.Event{ID: trace.EventID(id, action), Kind: "local-effect", Body: action}
		event.Operation = &coreadapter.Operation{ID: trace.OperationID(projectID, streamID, event.ID), Boundary: coreadapter.RunnerBoundary, Action: action, Input: json.RawMessage(`{}`)}
		events = append(events, event)
	}
	tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: id, Revision: 1, Project: projectID, Workstream: streamID, At: epoch, Actor: trace.Actor{Kind: "service", ID: "scheduler"}, Cause: "owner-request", Depth: 2}, Subject: id, To: "pending", Reason: "Dispatched sessions"}, Events: events}
	_, err := f.repository.Transact(context.Background(), tx)
	must(t, err)
}

// concurrent is a controller over s whose operations named in actions are
// concurrent.
func (f *fixture) concurrent(t *testing.T, s *sessions, actions ...string) *Controller {
	t.Helper()
	c, err := New(f.repository, Options{Worker: "test-controller", Now: f.clock.Now, RetryDelay: time.Minute,
		Adapters:   map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: s},
		Concurrent: func(op coreadapter.Operation) bool { return slices.Contains(actions, op.Action) }})
	must(t, err)
	return c
}

// done reports, by action, whether its operation is acknowledged with a
// result, and fails unless each has at most one result and acknowledgement.
func (f *fixture) done(t *testing.T) map[string]bool {
	t.Helper()
	records, err := f.repository.Operations(streamID)
	must(t, err)
	out := map[string]bool{}
	for _, r := range records {
		results, acks := 0, 0
		for _, a := range r.History {
			switch a.Kind {
			case "result":
				results++
			case "acknowledge":
				acks++
			}
		}
		if results > 1 || acks > 1 {
			t.Fatalf("operation %s recorded %d results and %d acknowledgements", r.Operation.Action, results, acks)
		}
		out[r.Operation.Action] = r.Acknowledged && r.Result != nil
	}
	return out
}

// Two concurrent operations of one pass apply their effects at the same
// time: each effect waits for the other to start.
func TestConcurrentOperationsApplyAtOnce(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.publish(t, "first", "second")
	s := newSessions("first", "second")
	c := f.concurrent(t, s, "first", "second")
	c.options.Adapters[coreadapter.RepositoryBoundary] = f.system
	must(t, c.Pass(context.Background()))
	must(t, c.Wait())
	if got := f.done(t); !got["first"] || !got["second"] || !got["prepare"] {
		t.Fatalf("completed %v", got)
	}
	if _, _, applied := s.snapshot(); len(applied) != 2 {
		t.Fatalf("applied %v", applied)
	}
}

// A serial operation completes in a pass while a concurrent operation's
// effect is still running, and that pass leaves the running one alone.
func TestSerialOperationCompletesWhileAConcurrentEffectRuns(t *testing.T) {
	ctx := context.Background()
	f := setup(t, coreadapter.RepositoryBoundary)
	f.publish(t, "session")
	s := newSessions()
	c := f.concurrent(t, s, "session")
	c.options.Adapters[coreadapter.RepositoryBoundary] = f.system
	var mu sync.Mutex
	offered := 0
	c.boundary = func(step string) error {
		mu.Lock()
		defer mu.Unlock()
		if step == "before-claim" {
			offered++
		}
		return nil
	}
	must(t, c.Pass(ctx))
	f.awaitStarted(t, s, "session")
	f.publish(t, "landing")
	must(t, c.Pass(ctx))
	if got := f.done(t); got["session"] || !got["landing"] || !got["prepare"] {
		t.Fatalf("while the session runs: %v", got)
	}
	mu.Lock()
	if offered != 3 {
		t.Errorf("passes offered %d operations, want the first pass's two and the landing", offered)
	}
	mu.Unlock()
	if inspections, started, _ := s.snapshot(); !slices.Equal(started, []string{"session", "landing"}) || !slices.Equal(inspections, []string{"session", "landing"}) {
		t.Fatalf("started %v, inspected %v", started, inspections)
	}
	close(s.release)
	must(t, c.Wait())
	if got := f.done(t); !got["session"] || !got["landing"] {
		t.Fatalf("after release: %v", got)
	}
	if _, _, applied := s.snapshot(); !slices.Equal(applied, []string{"landing", "session"}) {
		t.Fatalf("applied %v", applied)
	}
}

// awaitStarted waits until the effect of the operation action started.
func (f *fixture) awaitStarted(t *testing.T, s *sessions, action string) {
	t.Helper()
	deadline := time.After(overlapTimeout)
	for {
		if _, started, _ := s.snapshot(); slices.Contains(started, action) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s never started", action)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Run cancels a concurrent operation in flight and joins it before it
// returns; the operation keeps its claim and completes after a restart.
func TestRunJoinsConcurrentOperations(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.publish(t, "session")
	s := newSessions()
	c := f.concurrent(t, s, "session")
	c.options.Adapters[coreadapter.RepositoryBoundary] = f.system
	c.options.Ticks = make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	f.awaitStarted(t, s, "session")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	s.mu.Lock()
	returned := s.returned["session"]
	s.mu.Unlock()
	if !returned {
		t.Fatal("run returned before the session")
	}
	if got := f.done(t); got["session"] {
		t.Fatalf("a cancelled session completed: %v", got)
	}
	f.reopen(t)
	close(s.release)
	c = f.concurrent(t, s, "session")
	must(t, c.Pass(context.Background()))
	must(t, c.Wait())
	if got := f.done(t); !got["session"] {
		t.Fatalf("after restart: %v", got)
	}
}

// A store failure of a concurrent operation stops the loop with that error.
func TestConcurrentOperationFailureStopsRun(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.publish(t, "session")
	s := newSessions()
	close(s.release)
	c := f.concurrent(t, s, "session")
	c.options.Adapters[coreadapter.RepositoryBoundary] = f.system
	c.options.Ticks = make(chan time.Time)
	c.boundary = func(step string) error {
		if step == "before-result" {
			return injected
		}
		return nil
	}
	errs := make(chan error, 1)
	go func() { errs <- c.Run(context.Background()) }()
	select {
	case err := <-errs:
		if !errors.Is(err, injected) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(overlapTimeout):
		t.Fatal("the failure did not stop the loop")
	}
	if err := c.Pass(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("pass after the failure: %v", err)
	}
}

// A concurrent operation interrupted at any boundary completes once after a
// restart, and a recorded result is not inspected again.
func TestRestartAtEveryBoundaryOfAConcurrentOperation(t *testing.T) {
	steps := []string{"before-claim", "after-claim", "before-inspection", "after-inspection", "after-observation", "before-effect", "after-effect", "before-result", "after-result", "before-acknowledgement", "after-acknowledgement"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			f := setup(t, coreadapter.RunnerBoundary)
			concurrent := func() *Controller {
				c := f.controller(t)
				c.options.Concurrent = func(coreadapter.Operation) bool { return true }
				return c
			}
			c := concurrent()
			c.boundary = func(name string) error {
				if name == step {
					return injected
				}
				return nil
			}
			err := c.Pass(context.Background())
			if step == "before-claim" {
				if !errors.Is(err, injected) {
					t.Fatalf("boundary %s: %v", step, err)
				}
			} else if must(t, err); !errors.Is(c.Wait(), injected) {
				t.Fatalf("boundary %s: %v", step, c.Wait())
			}
			before, _ := f.system.counts()
			f.reopen(t)
			for range 2 {
				c := concurrent()
				must(t, c.Pass(context.Background()))
				must(t, c.Wait())
			}
			f.completed(t)
			after, _ := f.system.counts()
			if (step == "after-result" || step == "before-acknowledgement" || step == "after-acknowledgement") && after != before {
				t.Fatal("terminal result was inspected again")
			}
		})
	}
}
