package service

import (
	"encoding/json"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

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
