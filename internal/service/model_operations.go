package service

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

// modelOperationRole names workflow runners whose capacity is reserved at
// operation admission, before their durable threads have claimed a turn.
func modelOperationRole(op coreadapter.Operation) string {
	if op.Boundary != coreadapter.RunnerBoundary {
		return ""
	}
	switch op.Action {
	case DraftAction, ReplyAction, RedraftAction, AmendmentDraftAction, AmendmentReplyAction:
		return architectRole
	case FinalReviewAction, deliveryReviewAction:
		return committeeRole
	}
	return ""
}

func (s *Service) modelOperationLimit(repository *trace.Repository, other func(coreadapter.Operation) (func(), bool)) func(coreadapter.Operation) (func(), bool) {
	return func(op coreadapter.Operation) (func(), bool) {
		var releaseOther func()
		if other != nil {
			var admitted bool
			releaseOther, admitted = other(op)
			if !admitted {
				return nil, false
			}
		}
		role := modelOperationRole(op)
		if role == "" {
			return releaseOther, true
		}
		release, admitted := s.reserveModelOperation(repository, op, role)
		if admitted && role == committeeRole {
			reservation := &reviewReservation{release: release, reserve: func() (func(), bool) {
				return s.reserveModelOperation(repository, op, role)
			}}
			if _, loaded := s.reviewSlots.LoadOrStore(op.ID, reservation); loaded {
				release()
				admitted = false
			} else {
				release = sync.OnceFunc(func() {
					reservation.close()
					s.reviewSlots.CompareAndDelete(op.ID, reservation)
				})
			}
		}
		if !admitted {
			if releaseOther != nil {
				releaseOther()
			}
			return nil, false
		}
		return func() {
			release()
			if releaseOther != nil {
				releaseOther()
			}
		}, true
	}
}

// reviewReservation can return committee capacity without ending the workflow
// operation that owns it. Closing fences reacquisition as well as releasing.
type reviewReservation struct {
	mu      sync.Mutex
	release func()
	reserve func() (func(), bool)
	closed  bool
}

func (r *reviewReservation) yield() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.release != nil {
		r.release()
		r.release = nil
	}
}

func (r *reviewReservation) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.release != nil {
		r.release()
		r.release = nil
	}
}

func (r *reviewReservation) tryReserve() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.release != nil {
		return true
	}
	var admitted bool
	r.release, admitted = r.reserve()
	return admitted
}

func (s *Service) acquireReviewCheckSlot(ctx context.Context, run CheckSlotRun, operation string) (func(), error) {
	value, ok := s.reviewSlots.Load(operation)
	if !ok {
		return s.checkSlots.acquire(ctx, run)
	}
	reservation := value.(*reviewReservation)
	reservation.yield()
	// Acquire checks first, then try committee without waiting. If committee
	// is full, return the check slot and rejoin its queue after a cancellable
	// delay. No waiter holds either pool while waiting for the other, so unit
	// checks and other reviews can progress. The workflow claim stays held.
	for {
		release, err := s.checkSlots.acquire(ctx, run)
		if err != nil {
			return nil, err
		}
		if reservation.tryReserve() {
			if err := ctx.Err(); err != nil {
				release()
				reservation.yield()
				return nil, err
			}
			return release, nil
		}
		release()
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) reserveModelOperation(repository *trace.Repository, op coreadapter.Operation, role string) (func(), bool) {
	stream, ok := s.modelOperationStream(repository, op)
	if !ok {
		return nil, false
	}
	cfg := s.about(repository)
	limits := cfg.Capacity
	limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
	agent := architectAgent
	if role == committeeRole {
		agent = committeeAgent(1)
	}
	release, admitted, err := s.pool.TryReserve(repository, stream, role, agent, &limits)
	if err != nil {
		s.log().Error("model operation capacity", "project", repository.Project(), "workstream", stream, "operation", op.ID, "error", err)
	}
	return release, admitted
}

// modelOperationStream returns the workstream holding op. A later lookup of
// the same operation, on each pass while it waits for capacity, reads only
// the workstream an earlier lookup found it in, and reads every workstream
// only when the operation is no longer there.
func (s *Service) modelOperationStream(repository *trace.Repository, op coreadapter.Operation) (config.WorkstreamID, bool) {
	key := string(repository.Project()) + "/" + op.ID
	if found, ok := s.modelOperationStreams.Load(key); ok {
		stream := found.(config.WorkstreamID)
		if s.holdsOperation(repository, stream, op) {
			return stream, true
		}
		s.modelOperationStreams.Delete(key)
	}
	streams, err := intake.NonSessionWorkstreams(repository)
	if err != nil {
		s.log().Error("model operation capacity", "operation", op.ID, "error", err)
		return "", false
	}
	for _, stream := range streams {
		if s.holdsOperation(repository, stream, op) {
			s.modelOperationStreams.Store(key, stream)
			return stream, true
		}
	}
	return "", false
}

// holdsOperation reports whether stream's workflow records op.
func (s *Service) holdsOperation(repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
	records, err := repository.Operations(stream)
	if err != nil {
		s.log().Error("model operation capacity", "project", repository.Project(), "workstream", stream, "operation", op.ID, "error", err)
		return false
	}
	return slices.ContainsFunc(records, func(record trace.OperationRecord) bool { return record.Operation.ID == op.ID })
}
