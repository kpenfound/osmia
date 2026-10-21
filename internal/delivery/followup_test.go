package delivery

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestPostMergeRepairCollectsBlockingProblemsInOneUnit(t *testing.T) {
	t.Parallel()
	_, repo := deliveryTrace(t)
	a := &FinalReviewer{Repository: repo}
	report := FinalReport{Review: 1, Spec: 1, Plan: 1,
		Criteria: []FinalCriterion{{Criterion: "spec#1", Gap: "no merged unit serves it"}, {Criterion: "spec#2", Evidence: "served by merged units dedupe"}},
		Findings: []FinalFinding{{Severity: FindingBlocking, Criteria: []string{"spec#2"}, Finding: "Dedupe breaks with the merged dependency."}, {Severity: FindingAdvisory, Finding: "Rename the helper."}}}
	added, found, err := a.Followup(stream, report, 1)
	fixturekit.Must(t, err)
	if !found || added.Unit.ID != "final-1" || !slices.Equal(added.Unit.Criteria, []string{"spec#1", "spec#2"}) || !slices.Equal(added.Unit.Footprint, []string{"internal.trace"}) ||
		!slices.Equal(added.Unit.Acceptance, []string{"Resolved: spec#1: no merged unit serves it", "Resolved: spec#2: Dedupe breaks with the merged dependency."}) || strings.Contains(added.Gap, "Rename") {
		t.Fatalf("the repair %+v", added)
	}
	report.Criteria[0] = FinalCriterion{Criterion: "spec#1", Evidence: "served by merged units resume"}
	report.Findings = report.Findings[1:]
	if added, found, err := a.Followup(stream, report, 1); err != nil || found {
		t.Fatalf("advisory findings built a repair %+v: %v", added, err)
	}
}
