package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *stallFixture) stallDocument(id string, revision int) trace.Header {
	return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: f.repo.Project(), Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: stallSeeder, Cause: "fixture"}
}

func (f *stallFixture) sealWorkstream(t *testing.T, spec, planContent string) {
	t.Helper()
	if planContent == "" {
		encoded, err := plan.Encode(plan.Plan{Version: plan.Version, Units: []plan.Unit{{ID: stallUnit, Title: "Stalled unit", Task: "Build it", Acceptance: []string{"It works."}, Criteria: []string{"spec#1"}}}})
		intakefixture.Must(t, err)
		planContent = string(encoded)
	}
	sealed, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(spec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: strings.Repeat("a", 40)}, Branch: "osmia/" + string(intakefixture.Stream), Workspace: "/nonexistent/" + string(intakefixture.Stream)})
	intakefixture.Must(t, err)
	intakefixture.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{
		{Header: f.stallDocument(plan.SpecDocument, 1), Path: plan.SpecPath, Content: spec},
		{Header: f.stallDocument(plan.PlanDocument, 1), Path: plan.PlanPath, Content: planContent},
		{Header: f.stallDocument(seal.DocumentID, 1), Path: seal.Path, Content: string(sealed)},
	}))
}

func (f *stallFixture) finalReport(t *testing.T, report api.FinalReport) {
	t.Helper()
	existing, err := delivery.FinalReports(f.repo, intakefixture.Stream)
	intakefixture.Must(t, err)
	data, err := json.Marshal(report)
	intakefixture.Must(t, err)
	intakefixture.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: f.stallDocument(delivery.FinalReportDocument, len(existing)+1), Path: delivery.FinalReportPath, Content: string(data) + "\n"}}))
}

func (f *stallFixture) move(t *testing.T, n int, note string) {
	t.Helper()
	data, err := json.Marshal(api.WorkstreamMove{Move: n, From: AssembledState, To: BuildingState, Note: note, By: "the chief of staff"})
	intakefixture.Must(t, err)
	intakefixture.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: f.stallDocument(lifecycle.WorkstreamMoveDocument(n), 1), Path: lifecycle.WorkstreamMovePath(n), Content: string(data) + "\n"}}))
}

func (f *stallFixture) stalledWorkstream(t *testing.T) (text, raw string) {
	t.Helper()
	f.transition(t, "stall-seed-assembled", trace.FeatureSubject, BuildingState, AssembledState, "", stallSeeder, fixturekit.DemoStart)
	f.quietFor(3 * time.Hour)
	rebase := f.queueOperation(t, "rebase-unit", f.clock.Now())
	for range f.s.GiveUpLimit().Failures {
		f.fail(t, rebase, "WATCHDOG-FAILURE the checkout is dirty")
		f.clock.Advance(2 * time.Minute)
	}
	exit, ok := f.pass(t, f.watch("45m"))[intake.TriggerStalled]
	if !ok {
		t.Fatal("the watchdog raised no stalled exit for the workstream")
	}
	res, err := f.s.RouteExit(context.Background(), exit)
	intakefixture.Must(t, err)
	if !res.Committed {
		t.Fatalf("the stalled workstream did not route: %+v", res)
	}
	var asked []systemone.Request
	for _, r := range f.jev.TransitionRequests() {
		if strings.Contains(requestOf(t, r), "ws-stalled") || strings.Contains(requestOf(t, r), "ws.stalled") {
			asked = append(asked, r)
		}
	}
	if len(asked) != 1 {
		t.Fatalf("Jev got %d transition requests for the stalled workstream, want 1", len(asked))
	}
	return summaryOf(t, asked[0]), requestOf(t, asked[0])
}

func requestOf(t *testing.T, r systemone.Request) string {
	t.Helper()
	data, err := json.Marshal(r)
	intakefixture.Must(t, err)
	return string(data)
}

func summaryOf(t *testing.T, r systemone.Request) string {
	t.Helper()
	state, err := json.Marshal(r.State)
	intakefixture.Must(t, err)
	var text string
	if json.Unmarshal(state, &text) == nil {
		state = []byte(text)
	}
	return string(state) + "\n" + requestOf(t, r)
}

func requireStalledIn(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("the request lacks %q", want)
		}
	}
}

func requireStalledNotIn(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(text, bad) {
			t.Errorf("the request carries %q", bad)
		}
	}
}

func TestAStalledWorkstreamExitCarriesTheApprovedIntentForJev(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	f.sealWorkstream(t, "# Spec\n\n## Intent\n\nSTALLED-INTENT keeps uploads resumable.\n\n```go\nfunc codeInIntent() {}\n```\n\n## Intended behaviour\n\nNOT-THE-INTENT\n\n## Acceptance criteria\n\n1. OUTCOME-ONE resumes uploads.\n2. OUTCOME-TWO speaks the widget protocol.\n", "")
	f.finalReport(t, api.FinalReport{Review: 7, Outcome: delivery.FinalReviewed, Findings: []api.FinalFinding{
		{Severity: delivery.FindingAdvisory, Finding: "ADVISORY-FINDING reads oddly."},
		{Severity: delivery.FindingBlocking, Criteria: []string{"spec#2"}, Finding: "BLOCKING-FINDING leaves a gap.", Action: "Close the gap."},
	}})
	f.move(t, 1, "EARLIER-MOVE-NOTE")

	text, _ := f.stalledWorkstream(t)
	requireStalledIn(t, text,
		"STALLED-INTENT", "OUTCOME-ONE", "OUTCOME-TWO", `"acceptance_outcomes":2`,
		"BLOCKING-FINDING", "ADVISORY-FINDING", "Close the gap.", `"findings":2`, `"blocking_findings":1`, "findings of final review 7",
		"EARLIER-MOVE-NOTE", `"earlier_moves":1`,
		`"minutes_quiet":`, `"last_progress":`, `"last_progress_was":`, `"operations_without_result":1`, "operation rebase-unit has no result", "WATCHDOG-FAILURE the checkout is dirty",
	)
	requireStalledNotIn(t, text, "sealed spec and plan are what the owner approved", "NOT-THE-INTENT", "codeInIntent")
	if strings.Index(text, "BLOCKING-FINDING") > strings.Index(text, "ADVISORY-FINDING") {
		t.Fatal("the advisory finding came before the blocking one")
	}
}

// A workstream whose spec was never sealed, or whose plan cannot be read, still
// stalls into a routed exit, and the request says what the records lack.
func TestAStalledWorkstreamExitWithMissingRecordsStillRoutes(t *testing.T) {
	t.Parallel()

	t.Run("never sealed", func(t *testing.T) {
		t.Parallel()
		f := newStallFixture(t, "", nil)
		text, _ := f.stalledWorkstream(t)
		requireStalledIn(t, text, "has no sealed spec or plan", "missing from the records", "no final review has read the workstream", `"operations_without_result":1`, "WATCHDOG-FAILURE")
		requireStalledNotIn(t, text, "sealed spec and plan are what the owner approved")
	})

	t.Run("plan that cannot be read", func(t *testing.T) {
		t.Parallel()
		f := newStallFixture(t, "", nil)
		f.sealWorkstream(t, "# Spec\n\n## Intent\n\nSTALLED-INTENT.\n\n## Acceptance criteria\n\n1. OUTCOME-ONE.\n", "this is not a plan")
		text, _ := f.stalledWorkstream(t)
		requireStalledIn(t, text, "STALLED-INTENT", "OUTCOME-ONE", "missing from the records", "plan.json revision 1", `"operations_without_result":1`)
	})
}

// With large records the request stays within 72 KiB, carries no code, counts
// what the records hold and says what was cut, beside the watchdog's evidence.
func TestAStalledWorkstreamExitOfLargeRecordsStaysBounded(t *testing.T) {
	t.Parallel()
	const code = "CODE-MARKER-FUNC"
	long := func(word string, n int) string { return strings.Repeat(word+" ", n) }
	f := newStallFixture(t, "", nil)
	var criteria []string
	for i := 1; i <= 300; i++ {
		criteria = append(criteria, fmt.Sprintf("%d. OUTCOME-%d %s\n```\n%s()\n```", i, i, long("outcome", 120), code))
	}
	f.sealWorkstream(t, "# Spec\n\n## Intent\n\n```go\n"+code+"()\n```\n\n"+long("INTENT", 3000)+"\n\n## Acceptance criteria\n\n"+strings.Join(criteria, "\n")+"\n", "")
	var findings []api.FinalFinding
	for i := 0; i < 400; i++ {
		findings = append(findings, api.FinalFinding{Severity: delivery.FindingAdvisory, Finding: fmt.Sprintf("FINDING-%d\n```\n%s()\n```\n%s", i, code, long("finding", 300))})
	}
	findings = append(findings, api.FinalFinding{Severity: delivery.FindingBlocking, Finding: "BLOCKING-LAST"})
	f.finalReport(t, api.FinalReport{Review: 7, Outcome: delivery.FinalReviewed, Findings: findings})
	for i := 1; i <= 40; i++ {
		f.move(t, i, fmt.Sprintf("MOVE-%d %s", i, long("note", 300)))
	}

	text, raw := f.stalledWorkstream(t)
	if len(raw) > jev.MaxDecisionBytes {
		t.Fatalf("the request is %d bytes, over %d", len(raw), jev.MaxDecisionBytes)
	}
	requireStalledIn(t, text, "[cut:", `"acceptance_outcomes":300`, `"findings":401`, `"blocking_findings":1`, `"earlier_moves":40`, `"earlier_moves_shown":10`,
		"BLOCKING-LAST", "MOVE-40", "[code omitted]", "30 oldest", "WATCHDOG-FAILURE", `"minutes_quiet":`)
	requireStalledNotIn(t, text, code, "MOVE-1 ")
	var answered bool
	for _, rec := range routeRecordsOf(t, f.repo, intakefixture.Stream) {
		if rec.Trigger == intake.TriggerStalled {
			answered = rec.Outcome != router.OutcomeFallback
		}
	}
	if !answered {
		t.Fatal("the request was too large to answer")
	}
}

// A stalled unit's exit carries the watchdog's evidence and
// the unit's own intent, and none of the workstream summary.
func TestAStalledUnitExitDoesNotCarryTheWorkstreamSummary(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	f.sealWorkstream(t, "# Spec\n\n## Intent\n\nSTALLED-INTENT.\n\n## Acceptance criteria\n\n1. OUTCOME-ONE.\n", "")
	f.quietFor(3 * time.Hour)
	exit, ok := f.pass(t, f.watch("45m"))[execution.TriggerStalled]
	if !ok {
		t.Fatal("the watchdog raised no stalled exit for the unit")
	}
	if strings.Contains(exit.Evidence.Intent, "STALLED-INTENT") || strings.Contains(exit.Evidence.Intent, "sealed spec and plan are what the owner approved") {
		t.Fatalf("the unit's intent is %q", exit.Evidence.Intent)
	}
	for _, r := range exit.Evidence.Records {
		if r.Kind == "sealed acceptance" || r.Kind == "final review findings" || r.Kind == "earlier move" {
			t.Fatalf("the unit exit carries the workstream summary: %+v", r)
		}
	}
	for _, key := range []string{"minutes_quiet", "last_progress", "operations_without_result"} {
		if _, ok := exit.Evidence.Facts[key]; !ok {
			t.Fatalf("the unit exit lacks the watchdog fact %q", key)
		}
	}
	if _, ok := exit.Evidence.Facts["acceptance_outcomes"]; ok {
		t.Fatal("the unit exit carries the workstream's counts")
	}
}
