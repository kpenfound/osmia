package verifytool

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

func scopeOf(role, unit string) coreadapter.Scope { return coreadapter.Scope{Role: role, Unit: unit} }

func TestTheSweepListsTheUnionOfFlowsOnceEachAndTellsTheReviewerToRunThemWithVerify(t *testing.T) {
	text := RenderSweep(SweepInput{Dir: "docs/verification", Commit: "abc123", Features: []verification.Feature{
		{Name: "Checkout", Flows: []string{"checkout", "coupon"}},
		{Name: "Admin", Flows: []string{"admin-login", "checkout"}},
		{Name: "Reports", Flows: []string{}},
	}, Notes: []verification.Note{{File: "docs/verification/bad.md", Message: "the header does not parse"}}})
	for _, want := range []string{"regression sweep", "abc123", "run each one with verify on the feature-branch head", "1. checkout (Checkout, Admin)", "2. coupon (Checkout)", "3. admin-login (Admin)", "Inconclusive is never a pass", "docs/verification/bad.md: the header does not parse"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the sweep lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "checkout (") != 1 || strings.Contains(text, "4. ") {
		t.Fatalf("a flow is listed twice:\n%s", text)
	}
}

func TestASweepOfAMapWithoutFlowsSaysThereIsNothingToSweep(t *testing.T) {
	text := RenderSweep(SweepInput{Dir: "docs/verification", Commit: "abc"})
	if !strings.Contains(text, "names no flows") {
		t.Fatal(text)
	}
}

func TestMeasuredReadsRerunsAndReviewerRunsAndIgnoresEveryOtherCaller(t *testing.T) {
	ran := Outcome{Result: verification.Result{Claims: []verification.Claim{{Name: "n", Status: verification.Pass, Flow: "f"}}}}
	limited := Outcome{Limitation: "timed out"}
	cases := []struct {
		name    string
		op      Operation
		source  string
		outcome string
		ok      bool
	}{
		{"rerun result", Operation{Rerun: &Rerun{Unit: "u", Disposition: RerunRan}, Outcome: &ran}, verification.SourceRerun, verification.OutcomeResult, true},
		{"rerun limited", Operation{Rerun: &Rerun{Unit: "u", Disposition: RerunRan}, Outcome: &limited}, verification.SourceRerun, verification.OutcomeLimited, true},
		{"rerun skipped", Operation{Rerun: &Rerun{Unit: "u", Disposition: RerunSkipped}}, verification.SourceRerun, verification.OutcomeSkipped, true},
		{"rerun refused", Operation{Rerun: &Rerun{Unit: "u", Disposition: RerunRefused}}, verification.SourceRerun, verification.OutcomeRefused, true},
		{"reviewer", Operation{Scope: scopeOf("reviewer", "u"), Outcome: &ran}, verification.SourceReviewer, verification.OutcomeResult, true},
		{"manager", Operation{Scope: scopeOf(trace.Manager, ""), Outcome: &ran}, "", "", false},
		{"final reviewer", Operation{Scope: scopeOf("committee", ""), Outcome: &ran}, "", "", false},
	}
	for _, c := range cases {
		run, ok := Measured(c.op)
		if ok != c.ok || run.Source != c.source || (ok && run.Outcome != c.outcome) {
			t.Errorf("%s: %+v %v", c.name, run, ok)
		}
	}
}
