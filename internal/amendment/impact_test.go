package amendment

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	impactStreamA config.WorkstreamID = "w_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	impactStreamB config.WorkstreamID = "w_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	impactStreamC config.WorkstreamID = "w_cccccccccccccccccccccccccccccccc"
	impactStreamD config.WorkstreamID = "w_dddddddddddddddddddddddddddddddd"
)

func impactPlan(t *testing.T, units ...plan.Unit) string {
	t.Helper()
	data, err := plan.Encode(plan.Plan{Version: plan.Version, Units: units})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func impactUnit(id string, n string, deps ...string) plan.Unit {
	return plan.Unit{ID: id, Task: "Implement " + id, Acceptance: []string{"The section holds"}, Criteria: []string{n}, Footprint: []string{"internal.trace"}, DependsOn: deps}
}

func TestContractDeltaPreservesUnchangedUnitsAndReordersRevisions(t *testing.T) {
	a, b := impactStreamA, impactStreamB
	old := trace.ProjectSpec{Behavior: "Upload", Contracts: []trace.Contract{{Name: "offset", Text: "integer"}}, Graph: []trace.Edge{{From: a, To: b, Type: trace.EdgeContract}}, Slices: []trace.Slice{
		{Workstream: a, Spec: "## Acceptance criteria\n1. Integer offset.\n2. Local logs.\n", Plan: impactPlan(t, impactUnit("contract", "spec#1"), impactUnit("logs", "spec#2")), ContractCriteria: []int{1}},
		{Workstream: b, Spec: "## Acceptance criteria\n1. Consume offset.\n2. Local UI.\n", Plan: impactPlan(t, impactUnit("consume", "spec#1"), impactUnit("ui", "spec#2")), ContractCriteria: []int{1}},
	}}
	data := old
	data.Contracts = []trace.Contract{{Name: "offset", Text: "string"}}
	data.Slices = slices.Clone(old.Slices)
	data.Slices[0].Spec = "## Acceptance criteria\n1. String offset.\n2. Local logs.\n3. Validate offset.\n"
	data.Slices[0].Plan = impactPlan(t, impactUnit("contract", "spec#1"), impactUnit("logs", "spec#2"), impactUnit("validate", "spec#3", "contract"))
	members := []Member{{Workstream: a, State: "building", Units: map[string]string{"contract": "merged", "logs": "merged"}}, {Workstream: b, State: "building", Units: map[string]string{"consume": "merged", "ui": "merged"}}}
	got, err := ClassifyImpact("delta", old, data, members)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range got.Streams {
		if row.Action != Rework || !row.Pause {
			t.Fatalf("stream impact %+v", row)
		}
		if row.Units[0].Action != Revise || row.Units[1].Action != Leave {
			t.Fatalf("unit impacts %+v", row.Units)
		}
	}
	if !got.Streams[1].Cascade {
		t.Fatal("dependent was not re-evaluated")
	}
	before, _ := plan.Parse([]byte(old.Slices[0].Plan))
	after, _ := plan.Parse([]byte(data.Slices[0].Plan))
	revised := RevisionPlan("delta", before, after, got.Streams[0])
	preserved, _ := revised.Unit("logs")
	original, _ := before.Unit("logs")
	if !reflect.DeepEqual(preserved, original) {
		t.Fatal("unaffected unit assignment changed")
	}
	added, _ := revised.Unit("validate")
	if !slices.Contains(added.DependsOn, RevisionID("delta", "contract")) {
		t.Fatalf("new unit bypassed revision: %+v", added)
	}
	if strings.Count(got.Summary(), "Impact summary") != 1 {
		t.Fatal("more than one summary")
	}
}

func TestWorkstreamImpactTable(t *testing.T) {
	for _, tc := range []struct {
		state  string
		pr     int
		merged bool
		want   string
	}{{"handed", 0, false, Replan}, {"ratified", 0, false, Replan}, {"building", 0, false, Rework}, {"delivered", 42, false, Rework}, {"delivered", 42, true, FollowUp}} {
		t.Run(tc.state+tc.want, func(t *testing.T) {
			sl := trace.Slice{Workstream: impactStreamA, Spec: "## Acceptance criteria\n1. Before.\n", Plan: impactPlan(t, impactUnit("unit", "spec#1"))}
			before := trace.ProjectSpec{Behavior: "Upload", Slices: []trace.Slice{sl}}
			sl.Spec = "## Acceptance criteria\n1. After.\n"
			after := trace.ProjectSpec{Behavior: "Upload", Slices: []trace.Slice{sl}}
			impact, err := ClassifyImpact("delta", before, after, []Member{{Workstream: impactStreamA, State: tc.state, PullRequest: tc.pr, Merged: tc.merged, Units: map[string]string{"unit": "merged"}}})
			if err != nil {
				t.Fatal(err)
			}
			if impact.Streams[0].Action != tc.want {
				t.Fatalf("impact %+v", impact)
			}
		})
	}
}

func TestRemovedSectionGetsRemovalRevision(t *testing.T) {
	before := plan.Plan{Version: plan.Version, Units: []plan.Unit{impactUnit("old", "spec#1"), impactUnit("keep", "spec#2", "old")}}
	after := plan.Plan{Version: plan.Version, Units: []plan.Unit{impactUnit("keep", "spec#2")}}
	row := StreamImpact{Units: []UnitImpact{{Unit: "old", Action: Remove, Revision: RevisionID("delta", "old")}, {Unit: "keep", Action: Leave}}}
	got := RevisionPlan("delta", before, after, row)
	removal, ok := got.Unit(RevisionID("delta", "old"))
	if !ok || !strings.Contains(removal.Task, "Remove the code") || !reflect.DeepEqual(removal.Footprint, before.Units[0].Footprint) {
		t.Fatalf("removal %+v", got)
	}
	for _, u := range got.Units {
		for _, d := range u.DependsOn {
			if _, ok := got.Unit(d); !ok {
				t.Fatalf("unknown dependency %s", d)
			}
		}
	}
}

func TestCancelCascadeLeavesMergedWorkAndUnrelatedMembers(t *testing.T) {
	members := []Member{{Workstream: impactStreamA, State: "building"}, {Workstream: impactStreamB, State: "building"}, {Workstream: impactStreamC, State: "delivered", Merged: true}, {Workstream: impactStreamD, State: "building"}}
	graph := []trace.Edge{{From: impactStreamA, To: impactStreamB, Type: trace.EdgeHardBlock}, {From: impactStreamB, To: impactStreamC, Type: trace.EdgeMergeOrder}}
	impact := CancelImpact([]config.WorkstreamID{impactStreamA}, graph, members)
	if impact.Streams[0].Action != Cancel || impact.Streams[1].Action != Cancel || !impact.Streams[1].Cascade || impact.Streams[2].Action != Leave || impact.Streams[3].Action != Leave {
		t.Fatalf("impact %+v", impact)
	}
}

func TestImpactRefusesMissingMemberAndCycles(t *testing.T) {
	sl := trace.Slice{Workstream: impactStreamA, Spec: "## Acceptance criteria\n1. Uploads retain their acknowledged offset.\n", Plan: impactPlan(t, impactUnit("unit", "spec#1"))}
	before := trace.ProjectSpec{Behavior: "Resume uploads from their acknowledged offset.", Slices: []trace.Slice{sl}}
	members := []Member{{Workstream: impactStreamA, State: "building"}}
	if err := trace.ValidateProjectSpec(before, []config.WorkstreamID{impactStreamA}); err != nil {
		t.Fatalf("invalid baseline project spec: %v", err)
	}
	baseline, err := ClassifyImpact("d", before, before, members)
	if err != nil {
		t.Fatalf("unchanged baseline refused: %v", err)
	}
	if len(baseline.Streams) != 1 || baseline.Streams[0].Action != Leave {
		t.Fatalf("unchanged baseline impact: %+v", baseline)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*trace.ProjectSpec)
		want   string
	}{
		{"missing slice", func(p *trace.ProjectSpec) { p.Slices = nil }, "amendment must cover member " + string(impactStreamA)},
		{"unknown member", func(p *trace.ProjectSpec) {
			p.Graph = []trace.Edge{{From: impactStreamA, To: impactStreamB, Type: trace.EdgeContract}}
		}, "edge names unknown workstream " + string(impactStreamB)},
		{"cycle", func(p *trace.ProjectSpec) {
			p.Graph = []trace.Edge{{From: impactStreamA, To: impactStreamA, Type: trace.EdgeContract}}
		}, "the graph has a cycle: " + string(impactStreamA) + " -> " + string(impactStreamA)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := before
			tc.mutate(&after)
			if _, err := ClassifyImpact("d", before, after, members); err == nil || err.Error() != tc.want {
				t.Fatalf("refusal = %v, want %q", err, tc.want)
			}
		})
	}
}
