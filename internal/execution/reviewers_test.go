package execution

import (
	context "context"
	json "encoding/json"

	strings "strings"

	testing "testing"

	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
)

func TestStaleReviewIdentifiesEachRevision(t *testing.T) {
	t.Parallel()
	current := UnitReviewIdentity{Candidate: coreadapter.Candidate{Revision: "candidate", BaseRevision: "base", SpecRevision: "1", PlanRevision: "2"}, DiffSHA256: "diff", Report: "report", Seal: 1}
	for _, tc := range []struct {
		name   string
		change func(*UnitReviewIdentity)
	}{
		{"candidate", func(i *UnitReviewIdentity) { i.Candidate.Revision = "old" }},
		{"base", func(i *UnitReviewIdentity) { i.Candidate.BaseRevision = "old" }},
		{"spec", func(i *UnitReviewIdentity) { i.Candidate.SpecRevision = "old" }},
		{"plan", func(i *UnitReviewIdentity) { i.Candidate.PlanRevision = "old" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewed := current
			tc.change(&reviewed)
			if got := staleReview(reviewed, current); !strings.Contains(got, tc.name) {
				t.Fatal(got)
			}
		})
	}
}

func TestReviewerVerdictToolAndOutcome(t *testing.T) {
	t.Parallel()
	scope := coreadapter.Scope{Workstream: "stream", Unit: "resume", Thread: "reviewer-resume", Turn: "review-1", Role: reviewerRole}
	reports := &reviewerReports{}
	tool := reports.tool(scope)
	for _, input := range []UnitVerdict{{Decision: "satisfactory"}, {Decision: "material_findings", Summary: reviewSummary}} {
		data, _ := json.Marshal(input)
		out, err := tool.Handle(context.Background(), data)
		if err != nil || !strings.Contains(string(out), `"recorded":false`) {
			t.Fatalf("accepted incomplete verdict %s: %s %v", data, out, err)
		}
	}
	good := UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}}
	data, _ := json.Marshal(good)
	out, err := tool.Handle(context.Background(), data)
	if err != nil || !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("verdict refused: %s %v", out, err)
	}
	turns := &verdictTurns{Turns: staticVerdictTurn{}, reports: reports}
	result, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: scope})
	if err != nil || result.Outcome == nil || result.Outcome.Status != verdictOutcome {
		t.Fatalf("outcome %+v %v", result.Outcome, err)
	}
	var got UnitVerdict
	if err := json.Unmarshal([]byte(result.Outcome.Report), &got); err != nil || got.Findings[0].Action != good.Findings[0].Action || got.Summary != reviewSummary {
		t.Fatalf("report %+v %v", got, err)
	}
}

func TestValidateVerdictNamesTheProblem(t *testing.T) {
	t.Parallel()
	finding := ReviewFinding{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}
	for _, tc := range []struct {
		name    string
		verdict UnitVerdict
		want    string
	}{
		{"satisfactory", UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}, ""},
		{"material findings", UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{finding}}, ""},
		{"unknown decision", UnitVerdict{Decision: "approve", Summary: reviewSummary}, "decision must be satisfactory or material_findings"},
		{"no summary", UnitVerdict{Decision: "satisfactory", Summary: " "}, "summary is required"},
		{"material without findings", UnitVerdict{Decision: "material_findings", Summary: reviewSummary}, "material findings are required"},
		{"satisfactory with findings", UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []ReviewFinding{finding}}, "satisfactory verdict cannot carry material findings"},
		{"finding without action", UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "The retry fails"}}}, "each finding needs severity, evidence and action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := validateVerdict(tc.verdict)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// staticVerdictTurn is a fake model session that always completes with no
// output; it never exercises a real reviewer's reasoning, so this test only
// checks that verdictTurns.Run reports the verdict already recorded through
// the tool, not that a model session actually produces one.
type staticVerdictTurn struct{}

func (staticVerdictTurn) Run(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{}, nil
}
