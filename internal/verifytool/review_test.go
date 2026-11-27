package verifytool

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/verification"
)

func rerunOp(index int, id string, result verification.Result) Operation {
	return Operation{RunID: id, Commit: "abc", Args: []string{"flow", "--name=x"}, Rerun: &Rerun{Unit: "u", Report: 1, Index: index, Disposition: RerunRan}, Outcome: &Outcome{Result: result}}
}

func TestRenderReviewShowsUnlistedFlowsRejectedReportsAndUncoveredFlows(t *testing.T) {
	tagged := rerunOp(1, "v1", verification.Result{Claims: []verification.Claim{
		{Name: "a", Status: verification.Pass, Flow: "checkout"},
		{Name: "b", Status: verification.Inconclusive, Flow: "mystery"},
		{Name: "c", Status: verification.Pass},
	}})
	rejected := rerunOp(2, "v2", verification.Rejection("report.json is not valid JSON"))
	skipped := Operation{Args: []string{"flow"}, Rerun: &Rerun{Unit: "u", Report: 1, Index: 6, Disposition: RerunSkipped, Reason: "only the first 5 are rerun"}}
	text := RenderReview(ReviewInput{
		Declared: 6,
		Reruns:   []Operation{tagged, rejected, skipped},
		Runs:     []Operation{tagged, rejected},
		Features: []verification.Feature{{Name: "Checkout", File: "docs/verification/checkout.md", Flows: []string{"checkout", "coupon"}}},
		Listed:   []string{"checkout", "coupon"},
		Flags:    []string{"the diff touches the lever"},
		Notes:    []verification.Note{{File: "docs/verification/x.md", Message: "the header does not parse"}},
	})
	for _, want := range []string{
		"flow checkout: pass (run v1: pass)",
		"flow coupon: no service-recorded run covered it",
		"Untagged claims",
		"[pass] c",
		"- mystery: inconclusive",
		"inconclusive, covers no flow: report.json is not valid JSON",
		"status: inconclusive (the report was rejected",
		"Declared invocation 6: skipped, no run",
		"Flag: the diff touches the lever",
		"Structure check: docs/verification/x.md: the header does not parse",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the review sections lack %q:\n%s", want, text)
		}
	}
}

func TestRenderReviewWithNoRunsSaysNoRunCoveredEachFlow(t *testing.T) {
	text := RenderReview(ReviewInput{Features: []verification.Feature{{Name: "Checkout", File: "f.md", Flows: []string{"checkout"}}}})
	if !strings.Contains(text, "flow checkout: no service-recorded run covered it") || !strings.Contains(text, "declared no invocations") {
		t.Fatal(text)
	}
}
