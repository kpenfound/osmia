package execution

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/verification"
)

func TestAUnitsMeasureCountsRerunsAndReviewerRunsByStatusAndRecordsTheMapFlag(t *testing.T) {
	installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "measure", sevenInvocations(), true)
	runChecks(t, f.s, repo, stream)
	report := currentReport(t, repo, stream, "resume")
	recordReviewerRun(t, repo, stream, "resume", report.Candidate, []string{"flow", "--name=checkout"})
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")

	doc, err := latestDocumentOf(repo, stream, UnitMeasureDocument("resume"))
	must(t, err)
	if doc.Revision < 1 || doc.Path != verification.UnitMeasurePath("resume") || doc.Unit != "resume" {
		t.Fatalf("%+v", doc.Header)
	}
	var m verification.UnitMeasure
	must(t, json.Unmarshal([]byte(doc.Content), &m))
	if m.Unit != "resume" || m.Candidate != report.Candidate || m.Review < 1 {
		t.Fatalf("%+v", m)
	}
	if m.Runs.Total() != 6 || m.Runs.Fail != 6 || m.Runs.Pass != 0 || m.Runs.Inconclusive != 0 {
		t.Fatalf("runs by status: %+v", m.Runs)
	}
	if m.Declared.Runs.Total() != 5 || m.Declared.Skipped != 2 || m.Declared.Refused != 0 || m.Declared.UntaggedClaims != 5 || m.Declared.UntaggedRuns != 0 {
		t.Fatalf("declared: %+v", m.Declared)
	}
	if !slices.Equal(m.Declared.Flows, []string{"checkout"}) || !slices.Equal(m.Reviewer.Flows, []string{"checkout"}) || m.Reviewer.Runs.Total() != 1 {
		t.Fatalf("flows: declared %v, reviewer %+v", m.Declared.Flows, m.Reviewer)
	}
	if !m.MapTouched || m.LeverTouched {
		t.Fatalf("touched: lever %v, map %v", m.LeverTouched, m.MapTouched)
	}
}

func TestAnInconclusiveRerunIsCountedInconclusiveAndAnUntaggedRerunCoversNoFlow(t *testing.T) {
	installLever(t, untaggedReport, `["checkout"]`)
	f, stream, repo := verifyingFixture(t, "measure-untagged", [][]string{flowCall("a")}, true)
	runChecks(t, f.s, repo, stream)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	doc, err := latestDocumentOf(repo, stream, UnitMeasureDocument("resume"))
	must(t, err)
	var m verification.UnitMeasure
	must(t, json.Unmarshal([]byte(doc.Content), &m))
	if m.Runs.Pass != 1 || m.Declared.UntaggedRuns != 1 || len(m.Declared.Flows) != 0 {
		t.Fatalf("%+v", m)
	}

	installLever(t, inconclusiveReport, `["coupon"]`)
	f, stream, repo = verifyingFixture(t, "measure-inconclusive", [][]string{flowCall("a")}, true)
	runChecks(t, f.s, repo, stream)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	doc, err = latestDocumentOf(repo, stream, UnitMeasureDocument("resume"))
	must(t, err)
	m = verification.UnitMeasure{}
	must(t, json.Unmarshal([]byte(doc.Content), &m))
	if m.Runs.Pass != 0 || m.Runs.Inconclusive != 1 || !slices.Equal(m.Declared.Flows, []string{"coupon"}) {
		t.Fatalf("%+v", m)
	}
}

func TestAProjectWithoutTheSettingRecordsNoUnitMeasure(t *testing.T) {
	f, stream, repo := newReviewFixture(t, "measure-none")
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	doc, err := latestDocumentOf(repo, stream, UnitMeasureDocument("resume"))
	must(t, err)
	if doc.Revision != 0 {
		t.Fatalf("%+v", doc.Header)
	}
}
