package execution

import (
	"fmt"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// unplannedUnit supplies the one unit a Hill Climb workstream builds, the
// evaluator unit, from its sealed climb spec. The workstream has no sealed
// plan for the unit machinery to read, so this is the adapter that lets the
// ordinary mason, reviewer, checks and landing run it.
func unplannedUnit(spec trace.Document, unit string) (plan.Unit, error) {
	if unit != climb.EvaluatorUnitID {
		return plan.Unit{}, fmt.Errorf("a Hill Climb workstream has no unit %q: its one unit is %s", unit, climb.EvaluatorUnitID)
	}
	s, err := climb.Parse([]byte(spec.Content), climb.Defaults{})
	if err != nil {
		return plan.Unit{}, fmt.Errorf("the sealed climb spec does not parse: %w", err)
	}
	return climb.EvaluatorUnit(s), nil
}
