package intake

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

func RefuseReport(format string, args ...any) (json.RawMessage, error) {
	return json.Marshal(struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
	}{false, fmt.Sprintf(format, args...)})
}

// SealedUnit returns the unit of the latest sealed plan or follow-up scope names.
func SealedUnit(repository *trace.Repository, scope coreadapter.Scope) (plan.Unit, error) {
	stream := config.WorkstreamID(scope.Workstream)
	latest, _, found, err := seal.Latest(repository, stream)
	if err != nil {
		return plan.Unit{}, err
	}
	if !found {
		return plan.Unit{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	if n, attempt := climb.AttemptNumber(scope.Unit); attempt {
		spec, _, err := climb.SealedSpec(repository, stream, latest.Revision.Spec, climb.Defaults{})
		if err != nil {
			return plan.Unit{}, err
		}
		return climb.AttemptPlanUnit(spec, n), nil
	}
	p, err := sealedPlanOf(repository, stream, latest)
	if err != nil {
		return plan.Unit{}, err
	}
	unit, ok := p.Unit(scope.Unit)
	if !ok {
		added, found, err := followup.Find(repository, stream, scope.Unit)
		if err != nil {
			return plan.Unit{}, err
		}
		if !found {
			return plan.Unit{}, fmt.Errorf("the sealed plan and follow-ups of workstream %s have no unit %s", stream, scope.Unit)
		}
		return added.Unit, nil
	}
	return unit, nil
}

// sealedPlanOf returns the plan of the workstream's latest seal. A kind with
// a plan reads its sealed plan.json. A kind without one, Hill Climb, has the
// one unit its evaluator step runs, made from the sealed climb spec.
func sealedPlanOf(repository *trace.Repository, stream config.WorkstreamID, latest seal.Seal) (plan.Plan, error) {
	if planned, err := repository.Planned(stream); err != nil {
		return plan.Plan{}, err
	} else if !planned {
		p, _, err := climb.SealedEvaluatorPlan(repository, stream, latest.Revision.Spec)
		return p, err
	}
	graph, err := SealedPlan(repository, stream, latest.Revision.Plan)
	if err != nil {
		return plan.Plan{}, err
	}
	return plan.Parse([]byte(graph.Content))
}

// SealedPlanOf is sealedPlanOf for the code that reads the sealed plan of a
// workstream of any kind.
func SealedPlanOf(repository *trace.Repository, stream config.WorkstreamID, latest seal.Seal) (plan.Plan, error) {
	return sealedPlanOf(repository, stream, latest)
}
