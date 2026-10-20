package service

import (
	"github.com/kpenfound/osmia/internal/coreadapter"
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
	case ExtractAction, RefreshAction:
		return librarianRole
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
	streams, err := repository.Workstreams()
	if err != nil {
		s.log().Error("model operation capacity", "operation", op.ID, "error", err)
		return nil, false
	}
	for _, stream := range streams {
		records, err := repository.Operations(stream)
		if err != nil {
			s.log().Error("model operation capacity", "project", repository.Project(), "workstream", stream, "operation", op.ID, "error", err)
			continue
		}
		for _, record := range records {
			if record.Operation.ID != op.ID {
				continue
			}
			cfg := s.about(repository)
			limits := cfg.Capacity
			limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
			agent := architectAgent
			if role == librarianRole {
				agent = librarianAgent
			} else if role == committeeRole {
				agent = committeeAgent(1)
			}
			release, admitted, err := s.pool.TryReserve(repository, stream, role, agent, &limits)
			if err != nil {
				s.log().Error("model operation capacity", "project", repository.Project(), "workstream", stream, "operation", op.ID, "error", err)
			}
			return release, admitted
		}
	}
	return nil, false
}
