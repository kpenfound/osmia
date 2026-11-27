package recordspans

import (
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/verification"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func intAttr(t *testing.T, s tracetest.SpanStub, key string) int64 {
	t.Helper()
	v, ok := value(s, key)
	if !ok {
		t.Fatalf("span %s lacks %s: %s", s.Name, key, describe(s))
	}
	return v.AsInt64()
}

func boolAttr(t *testing.T, s tracetest.SpanStub, key string) bool {
	t.Helper()
	v, ok := value(s, key)
	if !ok {
		t.Fatalf("span %s lacks %s: %s", s.Name, key, describe(s))
	}
	return v.AsBool()
}

func TestAUnitsMeasureIsSentWithItsRunCountsFlowCountsAndTouchedFlags(t *testing.T) {
	h := newHarness(t)
	m := verification.MeasureUnit("u1", []verification.MeasuredRun{
		{Source: verification.SourceRerun, Outcome: verification.OutcomeResult, Result: verification.Result{Claims: []verification.Claim{{Name: "a", Status: verification.Pass, Flow: "x"}}}},
		{Source: verification.SourceRerun, Outcome: verification.OutcomeResult, Result: verification.Result{Claims: []verification.Claim{{Name: "a", Status: verification.Inconclusive}}}},
		{Source: verification.SourceRerun, Outcome: verification.OutcomeSkipped},
		{Source: verification.SourceReviewer, Outcome: verification.OutcomeResult, Result: verification.Result{Claims: []verification.Claim{{Name: "b", Status: verification.Fail, Flow: "y"}}}},
	})
	m.Review, m.Candidate, m.MapTouched = 1, "abc", true
	h.document("u1-verification-measure", verification.UnitMeasurePath("u1"), minute(5), review, "u1", m)

	spans := named(h.spans(), "osmia.verify.unit")
	if len(spans) != 1 {
		t.Fatalf("%d unit measure spans", len(spans))
	}
	s := spans[0]
	want := map[string]int64{
		telemetry.KeyVerifyRuns: 3, telemetry.KeyVerifyPass: 1, telemetry.KeyVerifyFail: 1, telemetry.KeyVerifyInconclusive: 1, telemetry.KeyVerifyLimited: 0,
		telemetry.KeyVerifyDeclaredRuns: 2, telemetry.KeyVerifyDeclaredFlows: 1, telemetry.KeyVerifyDeclaredUntagged: 1,
		telemetry.KeyVerifyReviewerRuns: 1, telemetry.KeyVerifyReviewerFlows: 1, telemetry.KeyVerifyReviewerUntagged: 0,
		telemetry.KeyVerifySkipped: 1, telemetry.KeyVerifyRefused: 0,
	}
	for key, n := range want {
		if got := intAttr(t, s, key); got != n {
			t.Errorf("%s = %d, want %d", key, got, n)
		}
	}
	if boolAttr(t, s, telemetry.KeyVerifyLeverTouched) || !boolAttr(t, s, telemetry.KeyVerifyMapTouched) {
		t.Errorf("touched flags: %s", describe(s))
	}
	if str(s, telemetry.KeyUnit) != "u1" {
		t.Errorf("unit: %s", describe(s))
	}
}

func TestAFinalMeasureSendsFailedAndInconclusiveFlowsApartWithTheirMarkers(t *testing.T) {
	h := newHarness(t)
	failed := verification.Result{Claims: []verification.Claim{{Name: "a", Status: verification.Fail, Flow: "shared"}, {Name: "b", Status: verification.Fail, Flow: "orphan"}, {Name: "c", Status: verification.Fail}, {Name: "d", Status: verification.Inconclusive, Flow: "maybe"}}}
	m := verification.AttributeFinal(1, "abc", []verification.Result{failed}, 0, []verification.Association{
		{Unit: "u1", Flows: []string{"shared", "maybe"}}, {Unit: "u2", Flows: []string{"shared"}}, {Unit: "u3", Flows: []string{"other"}}})
	h.document("final-verification-1", verification.FinalMeasurePath(1), minute(50), service, "", m)

	spans := h.spans()
	final := named(spans, "osmia.verify.final")
	if len(final) != 1 {
		t.Fatalf("%d final spans", len(final))
	}
	s := final[0]
	for key, n := range map[string]int64{telemetry.KeyVerifyFailedFlows: 2, telemetry.KeyVerifyInconclusiveFlows: 1, telemetry.KeyVerifyUnattributedFails: 2, telemetry.KeyVerifyFailingUnits: 2, telemetry.KeyVerifyInconclusiveUnits: 1, telemetry.KeyVerifyRuns: 1, telemetry.KeyVerifyFail: 1, telemetry.KeyVerifyPass: 0} {
		if got := intAttr(t, s, key); got != n {
			t.Errorf("%s = %d, want %d", key, got, n)
		}
	}
	flows := map[string]tracetest.SpanStub{}
	for _, f := range named(spans, "osmia.verify.final_flow") {
		flows[str(f, telemetry.KeyVerifyFlow)] = f
	}
	if len(flows) != 3 {
		t.Fatalf("%d flow spans", len(flows))
	}
	if f := flows["shared"]; str(f, telemetry.KeyOutcome) != FlowFail || !boolAttr(t, f, telemetry.KeyVerifyShared) || boolAttr(t, f, telemetry.KeyVerifyUnattributed) || f.Status.Code.String() != "Error" {
		t.Errorf("shared: %s", describe(f))
	}
	if f := flows["orphan"]; str(f, telemetry.KeyOutcome) != FlowFail || boolAttr(t, f, telemetry.KeyVerifyShared) || !boolAttr(t, f, telemetry.KeyVerifyUnattributed) {
		t.Errorf("orphan: %s", describe(f))
	}
	if f := flows["maybe"]; str(f, telemetry.KeyOutcome) != FlowInconclusive || f.Status.Code.String() == "Error" || boolAttr(t, f, telemetry.KeyVerifyUnattributed) {
		t.Errorf("maybe: %s", describe(f))
	}
	var failing, inconclusive []string
	for _, u := range named(spans, "osmia.verify.final_unit") {
		switch str(u, telemetry.KeyOutcome) {
		case FlowFail:
			failing = append(failing, str(u, telemetry.KeyUnit))
		case FlowInconclusive:
			inconclusive = append(inconclusive, str(u, telemetry.KeyUnit))
		}
	}
	slices.Sort(failing)
	if !slices.Equal(failing, []string{"u1", "u2"}) || !slices.Equal(inconclusive, []string{"u1"}) {
		t.Errorf("failing %v inconclusive %v", failing, inconclusive)
	}
}

func TestOtherDocumentsSendNoVerificationSpans(t *testing.T) {
	h := newHarness(t)
	h.document("u1-features", "units/u1/features.json", minute(5), review, "u1", map[string]string{"unit": "u1"})
	h.document("verify-x", "tools/verify-x.json", minute(6), review, "", map[string]string{})
	for _, name := range []string{"osmia.verify.unit", "osmia.verify.final", "osmia.verify.final_flow", "osmia.verify.final_unit"} {
		if got := named(h.spans(), name); len(got) != 0 {
			t.Errorf("%d %s spans", len(got), name)
		}
	}
}
