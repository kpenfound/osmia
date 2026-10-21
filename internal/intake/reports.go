package intake

import (
	"encoding/json"
	"fmt"

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

// sealedUnit returns the unit of the latest sealed plan or follow-up scope names.
func SealedUnit(repository *trace.Repository, scope coreadapter.Scope) (plan.Unit, error) {
	stream := config.WorkstreamID(scope.Workstream)
	latest, _, found, err := seal.Latest(repository, stream)
	if err != nil {
		return plan.Unit{}, err
	}
	if !found {
		return plan.Unit{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	graph, err := SealedPlan(repository, stream, latest.Revision.Plan)
	if err != nil {
		return plan.Unit{}, err
	}
	p, err := plan.Parse([]byte(graph.Content))
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
