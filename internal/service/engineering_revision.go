package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// engineeringRevision checks the mechanical boundaries of delegated plan decisions.
// Whether an approach fulfills intent remains a recorded engineering judgment.
func engineeringRevision(r *trace.Repository, stream config.WorkstreamID, c amendmentCase, current seal.Seal, dissent []shed.Entry, note string) *APIError {
	refuse := func(s string) *APIError { return &APIError{Conflict, s} }
	if strings.TrimSpace(note) == "" {
		return refuse("an engineering decision requires its reasoning against approved intent")
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return &APIError{Internal, err.Error()}
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
	if spec == "" || c.spec.Content != spec {
		return refuse("changing approved intent requires an owner decision")
	}
	for _, e := range dissent {
		if e.Blocking {
			return refuse("a charter or owner objection requires an owner decision")
		}
	}
	before, err := plan.Parse([]byte(graph))
	if err != nil {
		return &APIError{Internal, err.Error()}
	}
	after, err := plan.Parse([]byte(c.plan.Content))
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
			return &APIError{Internal, err.Error()}
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
func revisionWorkersIdle(r *trace.Repository, stream config.WorkstreamID, units []string) (bool, error) {
	for _, unit := range units {
		for _, agent := range []string{masonAgent(unit), reviewerAgent(unit)} {
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

// assignmentHeld checks dependencies on every worker turn, including resumed work.
func assignmentHeld(r *trace.Repository, stream config.WorkstreamID, unit, role string) (bool, error) {
	current, _, found, err := seal.Latest(r, stream)
	if err != nil || !found {
		return false, err
	}
	doc, err := sealedPlan(r, stream, current.Revision.Plan)
	if err != nil {
		return false, err
	}
	graph, err := plan.Parse([]byte(doc.Content))
	if err != nil {
		return false, err
	}
	u, exists := graph.Unit(unit)
	if !exists {
		return false, nil
	}
	for _, id := range u.DependsOn {
		st, err := r.Workflow(stream, trace.UnitSubject(id))
		if err != nil {
			return false, err
		}
		if st.Value != UnitMerged {
			return true, nil
		}
	}
	threads, err := r.Threads(stream)
	if err != nil {
		return false, err
	}
	var running []string
	for _, th := range threads {
		if th.Identity.Unit == "" || th.Identity.Unit == unit || th.Identity.Role != masonRole {
			continue
		}
		if _, exists := graph.Unit(th.Identity.Unit); !exists {
			continue
		}
		for _, turn := range th.Turns {
			if turn.Claim != nil && turn.Response == nil && th.Status != "interrupted" {
				running = append(running, th.Identity.Unit)
				break
			}
		}
	}
	if len(running) > 0 && role == masonRole {
		entities, err := kb.Load(r)
		if err != nil {
			return false, err
		}
		decision, err := plan.DecideStart(graph, entities, unit, running)
		if err != nil {
			return false, err
		}
		if !decision.CanStart {
			return true, nil
		}
	}
	requests, err := trace.Read[trace.Amendment](r, stream)
	if err != nil {
		return false, err
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return false, err
	}
	for _, req := range requests {
		st, err := r.Workflow(stream, amendmentSubject(req.ID))
		if err != nil {
			return false, err
		}
		if st.Value != amendmentApproved && st.Value != amendmentResealed {
			continue
		}
		for _, d := range docs {
			if d.Path == "amendments/"+req.ID+"/affected.json" {
				var a amendmentAffected
				if err := json.Unmarshal([]byte(d.Content), &a); err != nil {
					return false, err
				}
				if slices.Contains(a.Units, unit) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
