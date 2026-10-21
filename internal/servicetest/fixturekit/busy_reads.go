package fixturekit

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// BlockedFactoryOperation is a repository reconciler that waits for Release.
// Its unreserved action runs inline so read endpoints can be exercised while
// the scheduling pass is occupied. Reads use the trace's short bookkeeping
// lock and generation cache; they must not wait for the effect to finish.
type BlockedFactoryOperation struct {
	Started chan struct{}
	Release chan struct{}
	once    sync.Once
}

func NewBlockedFactoryOperation() *BlockedFactoryOperation {
	return &BlockedFactoryOperation{Started: make(chan struct{}), Release: make(chan struct{})}
}

func (b *BlockedFactoryOperation) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "fake factory operation in progress"}, nil
}

func (b *BlockedFactoryOperation) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	b.once.Do(func() { close(b.Started) })
	select {
	case <-b.Release:
	case <-ctx.Done():
		return coreadapter.OperationResult{}, ctx.Err()
	}
	return coreadapter.OperationResult{Outcome: "complete", Evidence: "fake factory operation finished"}, nil
}

// RequestFactoryOperation durably records a RepositoryBoundary operation of
// streamID, under an action the service does not reserve for sealing,
// building, landing, rebasing, drift or publishing, so the reconcile
// controller routes it to the test's own fake adapter (sealing.go,
// repositoryAdapter.Apply) rather than to a real one.
func RequestFactoryOperation(t *testing.T, repository *trace.Repository, streamID config.WorkstreamID, eventID string) {
	t.Helper()
	event := trace.Event{ID: eventID, Kind: "local-effect", Body: "simulated factory work"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(Project, streamID, event.ID), Boundary: coreadapter.RepositoryBoundary, Action: "fake-factory-op", Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: eventID + "-start", Revision: 1, Project: Project, Workstream: streamID, At: DemoStart, Actor: ServiceActor, Cause: "owner"}
	_, err := repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "fake-" + eventID, To: "pending", Reason: "owner requested"}, Events: []trace.Event{event}})
	Must(t, err)
}

// BusyReadsConfig seeds the workstream for a blocked repository operation.
func BusyReadsConfig(t *testing.T) config.Options {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := ConfigAt(t, home)
	cfg, err := config.Load(opts)
	Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, DemoStart, ServiceActor)
	Must(t, err)
	Must(t, repository.CreateWorkstream(context.Background(), Stream, DemoStart, ServiceActor))
	Must(t, repository.Close())
	return opts
}
