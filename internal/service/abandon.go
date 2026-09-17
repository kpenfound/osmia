package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// AbandonedState is the feature state of an abandoned workstream.
	AbandonedState = "abandoned"
	// DeliveredState is the feature state of a delivered workstream.
	DeliveredState = "delivered"
	// abandonTransition identifies the one transition to abandoned.
	abandonTransition = "abandon"
	// cancelReason is recorded on every turn abandonment cancels.
	cancelReason = "the owner abandoned the workstream"
)

// abandonActor is the provenance of the turn results abandonment records.
var abandonActor = trace.Actor{Kind: "service", ID: "abandon"}

// abandoned reports whether the workstream's feature state is abandoned.
func abandoned(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, trace.FeatureSubject)
	return state.Value == AbandonedState, err
}

// abandon moves a workstream that is neither delivered nor abandoned to
// abandoned, cancels the turns running for it in this service and completes
// its other unfinished turns as cancelled. Nothing is deleted.
func (s *Service) abandon(ctx context.Context, raw string, req AbandonRequest) (AbandonResponse, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return AbandonResponse{}, api
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return AbandonResponse{}, &APIError{Validation, "reason must not be empty"}
	}
	failed := &APIError{Internal, fmt.Sprintf("cannot abandon workstream %s; check the trace repository", stream)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: project, Workstream: stream, At: s.now(), Actor: ownerActor, Cause: abandonTransition}
	state, err := repository.SetFeatureStateUnless(ctx, h, AbandonedState, reason, AbandonedState, DeliveredState)
	switch {
	case errors.Is(err, trace.ErrFeatureState):
		return AbandonResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s is %s and cannot be abandoned", stream, state.Value)}
	case errors.Is(err, trace.ErrConflict):
		return AbandonResponse{}, &APIError{Conflict, fmt.Sprintf("workstream %s changed state while abandoning it; check osmia status and retry", stream)}
	case err != nil:
		return AbandonResponse{}, failed
	}
	s.turns.cancel(stream)
	if _, err := repository.CancelTurns(context.WithoutCancel(ctx), stream, s.now(), abandonActor, cancelReason); err != nil {
		return AbandonResponse{}, &APIError{Internal, fmt.Sprintf("workstream %s is abandoned but its queued turns could not be cancelled; they are cancelled at the next start. Check the trace repository", stream)}
	}
	return AbandonResponse{Project: project, Workstream: stream, State: AbandonedState, Reason: reason}, nil
}

// cancelAbandoned completes the unfinished turns of every abandoned
// workstream, finishing an abandonment a stopped service left partway.
func cancelAbandoned(ctx context.Context, repository *trace.Repository, at func() time.Time) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		gone, err := abandoned(repository, stream)
		if err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
		if !gone {
			continue
		}
		if _, err := repository.CancelTurns(ctx, stream, at(), abandonActor, cancelReason); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
	}
	return nil
}

// runningTurns holds the cancel functions of the turn operations this service
// is applying, by workstream.
type runningTurns struct {
	mu      sync.Mutex
	next    int
	streams map[config.WorkstreamID]map[int]context.CancelFunc
}

func (r *runningTurns) add(stream config.WorkstreamID, cancel context.CancelFunc) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams == nil {
		r.streams = map[config.WorkstreamID]map[int]context.CancelFunc{}
	}
	if r.streams[stream] == nil {
		r.streams[stream] = map[int]context.CancelFunc{}
	}
	r.next++
	id := r.next
	r.streams[stream][id] = cancel
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.streams[stream], id)
	}
}

func (r *runningTurns) cancel(stream config.WorkstreamID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cancel := range r.streams[stream] {
		cancel()
	}
}

// abandonable runs turn operations through the bound thread reconciler under
// a context that abandoning their workstream cancels. A turn of an abandoned
// workstream is completed as cancelled instead of being run, and a turn that
// finishes after its workstream was abandoned completes the thread's later
// turns as cancelled.
type abandonable struct {
	coreadapter.Reconciler
	s          *Service
	repository *trace.Repository
}

func (a abandonable) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := thread.DecodeTurn(op)
	if err != nil {
		return a.Reconciler.Apply(ctx, op)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer a.s.turns.add(in.Workstream, cancel)()
	gone, err := abandoned(a.repository, in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if gone {
		if _, err := a.repository.CancelTurns(ctx, in.Workstream, a.s.now(), abandonActor, cancelReason); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	result, err := a.Reconciler.Apply(ctx, op)
	// Abandoning while this turn ran left the thread's later turns queued.
	if gone, _ := abandoned(a.repository, in.Workstream); gone {
		if _, cancelErr := a.repository.CancelTurns(context.WithoutCancel(ctx), in.Workstream, a.s.now(), abandonActor, cancelReason); cancelErr != nil {
			return result, errors.Join(err, cancelErr)
		}
	}
	return result, err
}
