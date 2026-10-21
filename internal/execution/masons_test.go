package execution

import (
	slices "slices"

	config "github.com/kpenfound/osmia/internal/config"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	runtime "github.com/kpenfound/osmia/internal/runtime"

	trace "github.com/kpenfound/osmia/internal/trace"

	testing "testing"
	time "time"
)

// Units are taken in the plan's dependency order: a unit follows the units
// it depends on, and otherwise keeps its place in the plan.
func TestNextReadyFollowsTheDependencyOrder(t *testing.T) {
	t.Parallel()
	unit := func(id string, deps ...string) plan.Unit { return plan.Unit{ID: id, DependsOn: deps} }
	p := plan.Plan{Units: []plan.Unit{unit("c", "a"), unit("b"), unit("a"), unit("d")}}
	states := func(ready ...string) map[string]trace.WorkflowState {
		out := map[string]trace.WorkflowState{trace.UnitSubject("a"): {Value: UnitMerged}}
		for _, id := range ready {
			out[trace.UnitSubject(id)] = trace.WorkflowState{Value: UnitReady}
		}
		return out
	}
	for _, tc := range []struct {
		ready []string
		want  string
	}{{[]string{"c", "b"}, "b"}, {[]string{"c", "d"}, "c"}, {[]string{"d"}, "d"}, {nil, ""}} {
		got, ok, err := nextReady(building{plan: p, states: states(tc.ready...)}, kb.Map{})
		if err != nil || got != tc.want || ok != (tc.want != "") {
			t.Fatalf("ready %v: next %q %v %v, want %q", tc.ready, got, ok, err, tc.want)
		}
	}
}

// A ready unit entangled with a unit in flight is passed over for the next
// ready unit that is not, and a unit whose footprint does not resolve waits
// for every unit in flight.
func TestNextReadySkipsEntangledUnits(t *testing.T) {
	t.Parallel()
	entities := kb.Map{Version: kb.Version, Entities: []kb.Entity{
		{ID: "internal.trace", Name: "internal/trace", Aliases: []string{}, Paths: []string{"internal/trace"}, Owners: []string{}, PartOf: []string{}},
		{ID: "internal.upload", Name: "internal/upload", Aliases: []string{}, Paths: []string{"internal/upload"}, Owners: []string{}, PartOf: []string{}},
	}}
	unit := func(id string, footprint ...string) plan.Unit { return plan.Unit{ID: id, Footprint: footprint} }
	p := plan.Plan{Units: []plan.Unit{unit("a", "internal.trace"), unit("c", "internal.trace"), unit("d", "unknown"), unit("b", "internal.upload")}}
	for _, tc := range []struct {
		inFlight, ready []string
		want            string
	}{
		{nil, []string{"a", "c", "d", "b"}, "a"},
		{[]string{"a"}, []string{"c", "d", "b"}, "b"},
		{[]string{"a"}, []string{"c", "d"}, ""},
		{[]string{"b"}, []string{"d", "c"}, "c"},
		{nil, []string{"d"}, "d"},
	} {
		states := map[string]trace.WorkflowState{}
		for _, id := range tc.inFlight {
			states[trace.UnitSubject(id)] = trace.WorkflowState{Value: UnitImplementing}
		}
		for _, id := range tc.ready {
			states[trace.UnitSubject(id)] = trace.WorkflowState{Value: UnitReady}
		}
		got, ok, err := nextReady(building{plan: p, states: states, inFlight: tc.inFlight}, entities)
		if err != nil || got != tc.want || ok != (tc.want != "") {
			t.Fatalf("in flight %v, ready %v: next %q %v %v, want %q", tc.inFlight, tc.ready, got, ok, err, tc.want)
		}
	}
}

// Workstreams are offered a slot in the project's priority order, those it
// does not name last; among equals, the one that started a unit least
// recently goes first, then the lower ID.
func TestStartOrderIsPriorityThenRoundRobin(t *testing.T) {
	t.Parallel()
	at := func(minutes int) time.Time { return demoStart.Add(time.Duration(minutes) * time.Minute) }
	streams := []building{{stream: "a", started: at(2)}, {stream: "b", started: at(1)}, {stream: "c", started: at(3)}, {stream: "d"}, {stream: "e"}}
	priorities := []runtime.Priority{{Project: "other", Workstreams: []config.WorkstreamID{"a"}}, {Project: project, Workstreams: []config.WorkstreamID{"c"}}}
	var got []config.WorkstreamID
	for _, b := range startOrder(streams, priorities, project) {
		got = append(got, b.stream)
	}
	if want := []config.WorkstreamID{"c", "d", "e", "b", "a"}; !slices.Equal(got, want) {
		t.Fatalf("start order %v, want %v", got, want)
	}
}
