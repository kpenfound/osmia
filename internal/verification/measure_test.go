package verification

import (
	"slices"
	"testing"
)

func claims(flow string, statuses ...Status) Result {
	var r Result
	for _, s := range statuses {
		r.Claims = append(r.Claims, Claim{Name: "claim", Status: s, Flow: flow})
	}
	return r
}

func TestMeasureUnitCountsRunsByStatusAndKeepsInconclusiveApartFromPass(t *testing.T) {
	m := MeasureUnit("u", []MeasuredRun{
		{Source: SourceRerun, Outcome: OutcomeResult, Result: claims("x", Pass)},
		{Source: SourceRerun, Outcome: OutcomeResult, Result: claims("x", Pass, Inconclusive)},
		{Source: SourceRerun, Outcome: OutcomeResult, Result: Rejection("malformed")},
		{Source: SourceRerun, Outcome: OutcomeSkipped},
		{Source: SourceRerun, Outcome: OutcomeRefused},
		{Source: SourceReviewer, Outcome: OutcomeResult, Result: claims("y", Fail)},
		{Source: SourceReviewer, Outcome: OutcomeResult, Result: claims("", Pass)},
		{Source: SourceReviewer, Outcome: OutcomeLimited},
		{Source: "chief", Outcome: OutcomeResult, Result: claims("z", Pass)},
	})
	if m.Runs != (Counts{Pass: 2, Fail: 1, Inconclusive: 2}) || m.Limited != 1 {
		t.Fatalf("runs %+v limited %d", m.Runs, m.Limited)
	}
	if m.Declared.Runs != (Counts{Pass: 1, Inconclusive: 2}) || m.Declared.Skipped != 1 || m.Declared.Refused != 1 || m.Declared.Rejected != 1 || m.Declared.UntaggedRuns != 0 {
		t.Fatalf("declared %+v", m.Declared)
	}
	if !slices.Equal(m.Declared.Flows, []string{"x"}) || !slices.Equal(m.Reviewer.Flows, []string{"y"}) {
		t.Fatalf("flows %v %v", m.Declared.Flows, m.Reviewer.Flows)
	}
	if m.Reviewer.UntaggedRuns != 1 || m.Reviewer.UntaggedClaims != 1 || m.Reviewer.Limited != 1 {
		t.Fatalf("reviewer %+v", m.Reviewer)
	}
}

func TestMeasureUnitNeverCountsAFlowFromARunWithoutTags(t *testing.T) {
	m := MeasureUnit("u", []MeasuredRun{{Source: SourceRerun, Outcome: OutcomeResult, Result: claims("", Pass)}})
	if len(m.Declared.Flows) != 0 || m.Declared.UntaggedRuns != 1 {
		t.Fatalf("%+v", m.Declared)
	}
}

func associations() []Association {
	return []Association{{Unit: "A", Flows: []string{"x"}}, {Unit: "B", Flows: []string{"y"}}}
}

func TestAFailedFlowIsAttributedOnlyToTheUnitWhoseAssociationNamesIt(t *testing.T) {
	m := AttributeFinal(1, "c", []Result{claims("x", Pass), claims("y", Fail)}, 0, associations())
	if !slices.Equal(m.UnitsFailing, []string{"B"}) || len(m.UnitsInconclusive) != 0 {
		t.Fatalf("%+v", m)
	}
	if len(m.Failed) != 1 || m.Failed[0].Flow != "y" || m.Failed[0].Shared || m.Failed[0].Unattributed || !slices.Equal(m.Failed[0].Units, []string{"B"}) {
		t.Fatalf("%+v", m.Failed)
	}
	if m.Unattributed() != 0 {
		t.Fatalf("%d", m.Unattributed())
	}
}

func TestAnInconclusiveFinalFlowIsAttributedAsItsOwnMeasureAndNeverAsAFailure(t *testing.T) {
	m := AttributeFinal(2, "c", []Result{claims("x", Inconclusive), claims("y", Pass)}, 0, associations())
	if len(m.UnitsFailing) != 0 || len(m.Failed) != 0 || !slices.Equal(m.UnitsInconclusive, []string{"A"}) {
		t.Fatalf("%+v", m)
	}
	if len(m.Inconclusive) != 1 || m.Inconclusive[0].Flow != "x" || !slices.Equal(m.Inconclusive[0].Units, []string{"A"}) {
		t.Fatalf("%+v", m.Inconclusive)
	}
	if m.Runs.Pass != 1 || m.Runs.Inconclusive != 1 {
		t.Fatalf("%+v", m.Runs)
	}
}

func TestASharedFailureListsEveryUnitAndCountsOnceAtTheFlowLevel(t *testing.T) {
	assoc := append(associations(), Association{Unit: "C", Flows: []string{"y", "z"}})
	m := AttributeFinal(1, "c", []Result{claims("y", Pass, Fail), claims("y", Fail)}, 0, assoc)
	if len(m.Failed) != 1 || !m.Failed[0].Shared || !slices.Equal(m.Failed[0].Units, []string{"B", "C"}) {
		t.Fatalf("%+v", m.Failed)
	}
	if !slices.Equal(m.UnitsFailing, []string{"B", "C"}) {
		t.Fatalf("%v", m.UnitsFailing)
	}
}

func TestAFailedFlowNoUnitNamesAndAFailingUntaggedClaimAreUnattributed(t *testing.T) {
	untagged := Result{Claims: []Claim{{Name: "page loads", Status: Fail}, {Name: "banner", Status: Inconclusive}}}
	m := AttributeFinal(1, "c", []Result{claims("orphan", Fail), untagged}, 1, associations())
	if len(m.Failed) != 1 || !m.Failed[0].Unattributed || len(m.Failed[0].Units) != 0 || m.Failed[0].Shared {
		t.Fatalf("%+v", m.Failed)
	}
	if !slices.Equal(m.UnattributedClaims, []string{"page loads"}) || m.Unattributed() != 2 || m.Limited != 1 {
		t.Fatalf("%+v", m)
	}
	if len(m.UnitsFailing) != 0 {
		t.Fatalf("%v", m.UnitsFailing)
	}
}

func TestACombinedFlowTakesTheWorstStatusAcrossTheRoundsRuns(t *testing.T) {
	m := AttributeFinal(1, "c", []Result{claims("x", Pass), claims("x", Inconclusive), claims("x", Fail)}, 0, associations())
	if len(m.Failed) != 1 || len(m.Inconclusive) != 0 || len(m.Flows) != 1 || m.Flows[0].Status != Fail {
		t.Fatalf("%+v", m)
	}
}

func TestMeasurePathsAreRecognised(t *testing.T) {
	if u, ok := ParseUnitMeasurePath(UnitMeasurePath("resume")); !ok || u != "resume" {
		t.Fatal("unit measure path")
	}
	for _, p := range []string{"units/a/b/verification-measure.json", "units/a/features.json", "final/verification-x.json"} {
		if _, ok := ParseUnitMeasurePath(p); ok || IsFinalMeasurePath(p) {
			t.Fatalf("%s", p)
		}
	}
	if !IsFinalMeasurePath(FinalMeasurePath(3)) {
		t.Fatal("final measure path")
	}
}
