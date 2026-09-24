package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type interruptedRunner struct {
	mu                   sync.Mutex
	result               *coreadapter.OperationResult
	applies, inspections int
	applied              chan struct{}
}

func (r *interruptedRunner) Inspect(_ context.Context, _ coreadapter.Operation) (coreadapter.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inspections++
	if r.result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "session directory", Result: r.result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no session"}, nil
}
func (r *interruptedRunner) Apply(ctx context.Context, _ coreadapter.Operation) (coreadapter.OperationResult, error) {
	r.mu.Lock()
	r.applies++
	r.result = &coreadapter.OperationResult{Outcome: "finished", Evidence: "owned session result"}
	r.mu.Unlock()
	close(r.applied)
	<-ctx.Done()
	return coreadapter.OperationResult{}, ctx.Err()
}

func TestServiceResumesClaimedOperationWithoutWakeup(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	actor := trace.Actor{Kind: "service", ID: "test"}
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, now, actor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, now, actor))
	event := trace.Event{ID: "turn", Kind: "local-effect", Body: "authorized turn"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, stream, event.ID), Boundary: coreadapter.RunnerBoundary, Action: "run", Input: json.RawMessage(`{}`)}
	_, err = repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "start", Revision: 1, Project: project, Workstream: stream, At: now, Actor: actor, Cause: "owner"}, Subject: "turn", To: "pending", Reason: "owner requested"}, Events: []trace.Event{event}})
	must(t, err)
	must(t, repository.Close())
	runner := &interruptedRunner{applied: make(chan struct{})}
	ticks := make(chan time.Time)
	opts.Reconciliation = reconcile.Options{Now: func() time.Time { return now }, Ticks: ticks, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: runner}}
	s, err := Start(context.Background(), opts)
	must(t, err)
	select {
	case <-runner.applied:
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not apply")
	}
	must(t, s.Close())
	repository, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	records, err := repository.Operations(stream)
	must(t, err)
	if len(records) != 1 || records[0].Claim == nil || records[0].Result != nil {
		t.Fatalf("not interrupted: %+v", records)
	}
	must(t, repository.Close())
	s, err = Start(context.Background(), opts)
	must(t, err)
	// The loop accepts its first tick only after finishing the startup pass.
	select {
	case ticks <- now:
	case <-time.After(10 * time.Second):
		t.Fatal("restart did not finish startup pass")
	}
	must(t, s.Close())
	repository, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repository.Close()
	records, err = repository.Operations(stream)
	must(t, err)
	if !records[0].Acknowledged || records[0].Result == nil {
		t.Fatal(records)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.applies != 1 || runner.inspections != 2 {
		t.Fatalf("applies=%d inspections=%d", runner.applies, runner.inspections)
	}
}

func TestServiceRejectsLockedTrace(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, time.Now(), trace.Actor{Kind: "service", ID: "test"})
	must(t, err)
	defer repository.Close()
	s, err := Start(context.Background(), opts)
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, trace.ErrLocked) {
		t.Fatalf("locked trace was not rejected: %v", err)
	}
}

type forbiddenRunner struct{ t *testing.T }

func (f forbiddenRunner) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	f.t.Error("replaced runner adapter was inspected")
	return coreadapter.Observation{}, errors.New("forbidden")
}
func (f forbiddenRunner) Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error) {
	f.t.Error("replaced runner adapter was applied")
	return coreadapter.OperationResult{}, errors.New("forbidden")
}

type completedRunner struct{ inspections int }

func (c *completedRunner) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	c.inspections++
	return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "threads", Result: &coreadapter.OperationResult{Outcome: "from-threads", Evidence: "threads"}}, nil
}
func (c *completedRunner) Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error) {
	return coreadapter.OperationResult{}, errors.New("completed operations are not applied")
}

func runnerIntent(t *testing.T, opts Options) config.Root {
	t.Helper()
	cfg, err := config.Load(opts.Config)
	must(t, err)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	actor := trace.Actor{Kind: "service", ID: "test"}
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, now, actor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, now, actor))
	event := trace.Event{ID: "turn", Kind: "local-effect", Body: "authorized turn"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, stream, event.ID), Boundary: coreadapter.RunnerBoundary, Action: "run", Input: json.RawMessage(`{}`)}
	_, err = repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "start", Revision: 1, Project: project, Workstream: stream, At: now, Actor: actor, Cause: "owner"}, Subject: "turn", To: "pending", Reason: "owner requested"}, Events: []trace.Event{event}})
	must(t, err)
	must(t, repository.Close())
	return cfg.Root
}

func TestServiceThreadsErrorReleasesTrace(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	runnerIntent(t, opts)
	failure := errors.New("threads unavailable")
	var bound *trace.Repository
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		bound = r
		return nil, failure
	}
	s, err := Start(context.Background(), opts)
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, failure) || bound == nil {
		t.Fatalf("start with failing threads binding: %v", err)
	}
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	must(t, repository.Close())
	opts.Threads = nil
	s, err = Start(context.Background(), opts)
	must(t, err)
	must(t, s.Close())
}

func TestServiceThreadsReplaceRunnerAdapter(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	runnerIntent(t, opts)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time)
	forbidden := forbiddenRunner{t}
	adapters := map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: forbidden}
	opts.Reconciliation = reconcile.Options{Now: func() time.Time { return now }, Ticks: ticks, Adapters: adapters}
	threads := &completedRunner{}
	opts.Threads = func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) { return threads, nil }
	s, err := Start(context.Background(), opts)
	must(t, err)
	select {
	case ticks <- now:
	case <-time.After(5 * time.Minute):
		t.Fatal("startup pass did not finish")
	}
	must(t, s.Close())
	if len(adapters) != 1 || adapters[coreadapter.RunnerBoundary] != coreadapter.Reconciler(forbidden) {
		t.Fatalf("caller adapters changed: %v", adapters)
	}
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repository.Close()
	records, err := repository.Operations(stream)
	must(t, err)
	if threads.inspections != 1 || len(records) != 1 || !records[0].Acknowledged || records[0].Result == nil || records[0].Result.Outcome != "from-threads" {
		t.Fatalf("threads adapter did not handle the operation: %d %+v", threads.inspections, records)
	}
}

// orderedAdapter records the actions it applied, in order.
type orderedAdapter struct {
	mu      sync.Mutex
	applied []string
}

func (a *orderedAdapter) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not applied"}, nil
}
func (a *orderedAdapter) Apply(_ context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied = append(a.applied, op.Action)
	return coreadapter.OperationResult{Outcome: "finished", Evidence: "applied"}, nil
}

// A pass finishes work before it widens it, whichever workstream the work is
// in: everything else first, then the shed's rounds and replies, then drafts.
func TestServiceReconcilesOperationsInStageOrder(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	actor := trace.Actor{Kind: "service", ID: "test"}
	repository, err := trace.Create(ctx, cfg.Root, cfg.Project, now, actor)
	must(t, err)
	streams := []config.WorkstreamID{stream, "w_fedcba9876543210fedcba9876543210"}
	for i, id := range streams {
		must(t, repository.CreateWorkstream(ctx, id, now, actor))
		// Each workstream holds a draft, a reply, a round and a turn, so the
		// order workstreams are read in cannot produce the stage order.
		var events []trace.Event
		for _, action := range []string{DraftAction, ReplyAction, RoundAction, thread.TurnAction} {
			event := trace.Event{ID: fmt.Sprintf("%s-%d", action, i), Kind: "local-effect", Body: action}
			event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, id, event.ID), Boundary: coreadapter.ContainerBoundary, Action: action, Input: json.RawMessage(`{}`)}
			events = append(events, event)
		}
		_, err = repository.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "start", Revision: 1, Project: project, Workstream: id, At: now, Actor: actor, Cause: "owner"}, Subject: "work", To: "pending", Reason: "owner requested"}, Events: events})
		must(t, err)
	}
	must(t, repository.Close())
	adapter := &orderedAdapter{}
	ticks := make(chan time.Time)
	opts.Reconciliation = reconcile.Options{Now: func() time.Time { return now }, Ticks: ticks, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.ContainerBoundary: adapter}}
	s, err := Start(ctx, opts)
	must(t, err)
	// The loop accepts its first tick only after finishing the startup pass.
	select {
	case ticks <- now:
	case <-time.After(demoTimeout):
		t.Fatal("the startup pass did not finish")
	}
	must(t, s.Close())
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.applied) != 8 {
		t.Fatalf("applied %v", adapter.applied)
	}
	for i, action := range adapter.applied {
		stage := map[string]int{thread.TurnAction: 0, RoundAction: 1, ReplyAction: 1, DraftAction: 2}[action]
		if want := []int{0, 0, 1, 1, 1, 1, 2, 2}[i]; stage != want {
			t.Fatalf("operation %d is %s: %v", i+1, action, adapter.applied)
		}
	}
}

func TestServiceReportsScheduleFailure(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, time.Now(), trace.Actor{Kind: "service", ID: "test"})
	must(t, err)
	must(t, repository.Close())
	failure := errors.New("injected schedule failure")
	opts.Reconciliation.Schedule = func(context.Context) error { return failure }
	s, err := Start(context.Background(), opts)
	must(t, err)
	defer s.Close()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("service did not stop after schedule failure")
	}
	if err := s.Wait(); !errors.Is(err, failure) || !strings.Contains(err.Error(), "configured pass:") {
		t.Fatalf("service error = %v, want named pass and originating error", err)
	}
}
