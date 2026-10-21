package intake

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// engineeringRevision checks the mechanical boundaries of delegated plan decisions.
// Whether an approach fulfills intent remains a recorded engineering judgment.
func (s *Controller) EngineeringRevision(r *trace.Repository, stream config.WorkstreamID, c AmendmentCase, current seal.Seal, dissent []shed.Entry, note string) *APIError {
	refuse := func(s string) *APIError { return &APIError{Code: Conflict, Message: s} }
	if strings.TrimSpace(note) == "" {
		return refuse("an engineering decision requires its reasoning against approved intent")
	}
	// Final reviews run only once the feature has assembled, so their count
	// bounds revisions of an assembled workstream and of one reopened from it.
	rounds, err := s.FinalRoundsWithBlocking(r, stream)
	if err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	if rounds >= MaxFinalRounds {
		return refuse(fmt.Sprintf("final reviews have found blocking problems %d times; further revisions of the assembled plan require an owner decision", rounds))
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	var spec, graph string
	for _, d := range docs {
		if d.ID == plan.SpecDocument && d.Revision == current.Revision.Spec {
			spec = d.Content
		}
		if d.ID == plan.PlanDocument && d.Revision == current.Revision.Plan {
			graph = d.Content
		}
	}
	if spec == "" || c.Spec.Content != spec {
		return refuse("changing approved intent requires an owner decision")
	}
	for _, e := range dissent {
		if e.Blocking {
			return refuse("a charter or owner objection requires an owner decision")
		}
	}
	before, err := plan.Parse([]byte(graph))
	if err != nil {
		return &APIError{Code: Internal, Message: err.Error()}
	}
	after, err := plan.Parse([]byte(c.Plan.Content))
	if err != nil {
		return refuse("invalid proposed plan")
	}
	for _, old := range before.Units {
		next, exists := after.Unit(old.ID)
		if exists && reflect.DeepEqual(old, next) {
			continue
		}
		st, err := r.Workflow(stream, trace.UnitSubject(old.ID))
		if err != nil {
			return &APIError{Code: Internal, Message: err.Error()}
		}
		if st.Value == UnitMerged {
			return refuse("merged work is immutable; add a follow-up unit")
		}
		if !exists && st.Value != "" && st.Value != UnitPlanned && st.Value != UnitReady {
			return refuse("retain started unit identities; reassign remaining work explicitly")
		}
	}
	return nil
}

// revisionWorkersIdle prevents a new assignment from racing a worker using its predecessor.
func RevisionWorkersIdle(r *trace.Repository, stream config.WorkstreamID, units []string) (bool, error) {
	for _, unit := range units {
		for _, agent := range []string{MasonAgent(unit), ReviewerAgent(unit)} {
			th, err := r.Thread(stream, agent)
			if err != nil {
				// Units which have not started have no role thread.
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return false, fmt.Errorf("read %s: %w", agent, err)
			}
			for _, turn := range th.Turns {
				if turn.Claim != nil && turn.Response == nil && th.Status != "interrupted" {
					return false, nil
				}
			}
		}
	}
	return true, nil
}
