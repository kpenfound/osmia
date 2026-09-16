package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
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
