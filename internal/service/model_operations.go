package service

import (
	"slices"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

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
			if _, loaded := s.committeeAdmissions.LoadOrStore(op.ID, struct{}{}); loaded {
				release()
				admitted = false
			} else {
				reserved := release
				release = sync.OnceFunc(func() {
					reserved()
					s.committeeAdmissions.Delete(op.ID)
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

func (s *Service) holdsOperation(repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
	records, err := repository.Operations(stream)
	if err != nil {
		s.log().Error("model operation capacity", "project", repository.Project(), "workstream", stream, "operation", op.ID, "error", err)
		return false
	}
	return slices.ContainsFunc(records, func(record trace.OperationRecord) bool { return record.Operation.ID == op.ID })
}
