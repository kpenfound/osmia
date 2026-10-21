package execution

import (
	slices "slices"

	testing "testing"

	plan "github.com/kpenfound/osmia/internal/plan"

	trace "github.com/kpenfound/osmia/internal/trace"
)

// A planned unit becomes ready when the unit that merges is the last of its
// dependencies to merge, and not before.
func TestNewlyReadyWaitsForEveryDependency(t *testing.T) {
	t.Parallel()
	p := plan.Plan{Version: plan.Version, Units: []plan.Unit{
		{ID: "a"}, {ID: "b"},
		{ID: "both", DependsOn: []string{"a", "b"}},
		{ID: "only-a", DependsOn: []string{"a"}},
		{ID: "only-b", DependsOn: []string{"b"}},
	}}
	states := map[string]trace.WorkflowState{}
	for _, u := range p.Units {
		states[trace.UnitSubject(u.ID)] = trace.WorkflowState{Version: 1, Value: UnitPlanned}
	}
	states[trace.UnitSubject("a")] = trace.WorkflowState{Value: UnitApproved}
	states[trace.UnitSubject("b")] = trace.WorkflowState{Value: UnitApproved}
	ids := func(units []plan.Unit) []string {
		var out []string
		for _, u := range units {
			out = append(out, u.ID)
		}
		return out
	}
	if got := ids(newlyReady(p, states, "a")); !slices.Equal(got, []string{"only-a"}) {
		t.Fatalf("ready when a merges first: %v", got)
	}
	states[trace.UnitSubject("a")] = trace.WorkflowState{Value: UnitMerged}
	states[trace.UnitSubject("only-a")] = trace.WorkflowState{Value: UnitReady}
	if got := ids(newlyReady(p, states, "b")); !slices.Equal(got, []string{"both", "only-b"}) {
		t.Fatalf("ready when b merges after a: %v", got)
	}
}
