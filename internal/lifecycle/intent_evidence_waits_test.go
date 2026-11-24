package lifecycle_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	genericEdgeIntent = "A dependent workstream starts its work only after the workstreams it hard-depends on have merged."
	genericBaseIntent = "A workstream that builds on another workstream's branch waits while that base is abandoned, unsealed or missing."
)

func (f *edgeGraphFixture) edgeWaitCleared(t *testing.T, p *jevfixture.Transitions) string {
	t.Helper()
	routeJev(f.s, p)
	f.resolvePasses(t)
	f.recordOwnerMerge(t, f.upstream, "b1c2d3e4f5a6b1c2d3e4f5a6b1c2d3e4f5a6b1c2")
	f.resolvePasses(t)
	requests := p.TransitionRequests()
	if len(requests) != 1 {
		t.Fatalf("Jev got %d transition requests, want 1", len(requests))
	}
	if n := len(requestJSON(t, requests[0])); n > jev.MaxDecisionBytes {
		t.Fatalf("the request is %d bytes, over %d", n, jev.MaxDecisionBytes)
	}
	return requestText(t, requests[0])
}

func TestAnEdgeWaitExitCarriesTheApprovedIntentForJev(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	at := f.s.now()
	recordSealed(t, f.repository, f.downstream, at,
		"# Dependent\n\n## Intent\n\nDEPENDENT-INTENT keeps uploads resumable.\n\n```go\nfunc codeInIntent() {}\n```\n\n## Intended behaviour\n\nNOT-THE-INTENT\n\n## Acceptance criteria\n\n1. OUTCOME-ONE resumes uploads.\n2. OUTCOME-TWO speaks the widget protocol.\n",
		[]plan.Unit{
			unitOf("client", "Client resumes", "Resume against the endpoint of workstream "+string(f.upstream), "spec#1"),
			unitOf("wire", "Wire protocol", "Speak the protocol", "spec#2"),
			unitOf("docs", "Docs", "Write the documentation", "spec#1"),
		})
	recordSealed(t, f.repository, f.upstream, at, "# Upstream\n\n## Intent\n\nServe chunk uploads.\n\n## Acceptance criteria\n\n1. Chunks are stored.\n",
		[]plan.Unit{unitOf("endpoint", "Upstream endpoint", "Serve chunk uploads", "spec#1"), unitOf("auth", "Upstream auth", "Authenticate the clients", "spec#1")})
	recordFinalReport(t, f.repository, f.upstream, at, api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed, Summary: "UPSTREAM-SUMMARY says the endpoint is done",
		Findings: []api.FinalFinding{{Severity: delivery.FindingAdvisory, Finding: "The endpoint reads oddly."}}})
	groups, _, err := trace.Workgroups(f.repository)
	intakefixture.Must(t, err)
	intakefixture.Must(t, trace.RecordProjectSpec(ctx, f.repository, groups[0], trace.ProjectSpec{
		Behavior:  "Downstream depends on upstream.",
		Contracts: []trace.Contract{{Name: "widget-protocol", Text: "WIDGET-CONTRACT the upstream speaks widgets."}},
		Graph:     []trace.Edge{{From: f.upstream, To: f.downstream, Type: trace.EdgeHardBlock}},
		Slices:    []trace.Slice{{Workstream: f.downstream, ContractCriteria: []int{2}}},
	}, at, intake.OwnerActor, "fixture"))

	text := f.edgeWaitCleared(t, &jevfixture.Transitions{})
	requireIn(t, "the request", text,
		"DEPENDENT-INTENT", "OUTCOME-ONE", "OUTCOME-TWO", "spec#2",
		"WIDGET-CONTRACT", "widget-protocol", "contract sections and edges of the dependent",
		"unit client", "unit wire", `"relying_units":2`, `"dependent_plan_units":3`,
		"Upstream endpoint", "Upstream auth", `"upstream_planned_units":2`, `"dependent_acceptance_outcomes":2`,
		"UPSTREAM-SUMMARY", "was merged into", "its latest final report is review 1",
		`"blocking_edges":1`, `"observation_revision":`, "observed merged",
	)
	requireNotIn(t, "the request", text, genericEdgeIntent, "NOT-THE-INTENT", "codeInIntent", "unit docs (Docs)", "[cut:")
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerEdgeResolved); len(records) != 1 || records[0].State != router.StateCommitted {
		t.Fatalf("route %+v", records)
	}
}

func TestAnEdgeWaitExitWithMissingRecordsStillRoutes(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	text := f.edgeWaitCleared(t, &jevfixture.Transitions{})
	requireIn(t, "the request", text, "has no sealed spec or plan", "missing from the records", "the upstream has no plan", "no final report is on record", "contracts")
	requireNotIn(t, "the request", text, genericEdgeIntent)
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerEdgeResolved)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Outcome != router.OutcomeFollowed {
		t.Fatalf("the exit did not route: %+v", records)
	}
	if state := f.waitState(t); state.Value != "available" {
		t.Fatalf("the edge wait is %+v", state)
	}
}

func TestAnEdgeWaitExitOfLargeRecordsStaysBoundedAndSaysWhatWasCut(t *testing.T) {
	t.Parallel()
	const code = "CODE-MARKER-FUNC"
	long := func(word string, n int) string { return strings.Repeat(word+" ", n) }
	ctx := context.Background()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	at := f.s.now()
	var criteria []string
	var units, upstreamUnits []plan.Unit
	for i := 1; i <= 300; i++ {
		criteria = append(criteria, fmt.Sprintf("%d. OUTCOME-%d %s\n```\n%s()\n```", i, i, long("outcome", 120), code))
	}
	for i := 1; i <= 150; i++ {
		units = append(units, unitOf(fmt.Sprintf("dependent-%d", i), "Dependent unit", long("task", 200)+" names "+string(f.upstream), "spec#1"))
		upstreamUnits = append(upstreamUnits, unitOf(fmt.Sprintf("upstream-%d", i), "Upstream unit", "```\n"+code+"()\n```\n"+long("deliverable", 200), "spec#1"))
	}
	recordSealed(t, f.repository, f.downstream, at, "# Dependent\n\n## Intent\n\n```go\n"+code+"()\n```\n\n"+long("INTENT", 3000)+"\n\n## Acceptance criteria\n\n"+strings.Join(criteria, "\n")+"\n", units)
	recordSealed(t, f.repository, f.upstream, at, "# Upstream\n\n## Acceptance criteria\n\n1. Done.\n", upstreamUnits)
	var findings []api.FinalFinding
	for i := 0; i < 100; i++ {
		findings = append(findings, api.FinalFinding{Severity: delivery.FindingAdvisory, Finding: long("finding", 400)})
	}
	recordFinalReport(t, f.repository, f.upstream, at, api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed, Summary: long("summary", 1000), Findings: findings})
	var contracts []trace.Contract
	for i := 0; i < 60; i++ {
		contracts = append(contracts, trace.Contract{Name: fmt.Sprintf("contract-%d", i), Text: long("clause", 500)})
	}
	groups, _, err := trace.Workgroups(f.repository)
	intakefixture.Must(t, err)
	intakefixture.Must(t, trace.RecordProjectSpec(ctx, f.repository, groups[0], trace.ProjectSpec{Behavior: "Large.", Contracts: contracts,
		Graph: []trace.Edge{{From: f.upstream, To: f.downstream, Type: trace.EdgeHardBlock}}}, at, intake.OwnerActor, "fixture"))

	text := f.edgeWaitCleared(t, &jevfixture.Transitions{})
	requireIn(t, "the request", text, "[cut:", `"dependent_acceptance_outcomes":300`, `"contracts":60`, `"dependent_plan_units":150`, `"upstream_planned_units":150`, "[code omitted]", "shortened", `"blocking_edges":1`)
	requireNotIn(t, "the request", text, code, genericEdgeIntent)
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerEdgeResolved); len(records) != 1 || records[0].Outcome != router.OutcomeFollowed {
		t.Fatalf("the request was too large to answer: %+v", records)
	}
}

func TestABaseWaitExitCarriesTheApprovedIntentForJev(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)
	repo, stream := parkAndRelease(t, f)
	base, err := repo.WorkstreamBase(stream)
	intakefixture.Must(t, err)
	var findings []api.FinalFinding
	for i := 0; i < 100; i++ {
		findings = append(findings, api.FinalFinding{Severity: delivery.FindingAdvisory, Finding: strings.Repeat("finding ", 400)})
	}
	recordFinalReport(t, repo, base.Base, f.s.now(), api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed, Summary: "BASE-SUMMARY says the base was abandoned", Findings: findings})

	intakefixture.Must(t, f.s.baseWaitPass(context.Background(), repo))
	requests := p.TransitionRequests()
	if len(requests) != 1 {
		t.Fatalf("Jev got %d transition requests, want 1", len(requests))
	}
	if n := len(requestJSON(t, requests[0])); n > jev.MaxDecisionBytes {
		t.Fatalf("the request is %d bytes, over %d", n, jev.MaxDecisionBytes)
	}
	text := requestText(t, requests[0])
	spec := plan.ParseSpec(intakefixture.ValidSpec)
	if len(spec.Criteria) == 0 {
		t.Fatal("the fixture spec has no acceptance criteria")
	}
	for _, c := range spec.Criteria {
		requireIn(t, "the request", text, c.Text)
	}
	opening := ""
	for _, line := range strings.Split(intakefixture.ValidSpec, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			opening = line
			break
		}
	}
	graph, err := plan.Parse([]byte(intakefixture.ValidPlan))
	intakefixture.Must(t, err)
	if len(graph.Units) == 0 {
		t.Fatal("the fixture plan has no units")
	}
	for _, u := range graph.Units {
		requireIn(t, "the request", text, "unit "+u.ID, "builds on the base workstream's branch")
	}
	requireIn(t, "the request", text, opening,
		fmt.Sprintf(`"dependent_acceptance_outcomes":%d`, len(spec.Criteria)),
		fmt.Sprintf(`"relying_units":%d`, len(graph.Units)),
		fmt.Sprintf(`"upstream_planned_units":%d`, len(graph.Units)),
		"planned deliverables of the upstream", "the upstream is abandoned", "BASE-SUMMARY", "its latest final report is review 1",
		`"observation_revision":`, "is available")
	requireNotIn(t, "the request", text, genericBaseIntent)
	records := routeFor(t, repo, stream, "dep.base_available")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].State != router.StateCommitted {
		t.Fatalf("route %+v", records)
	}
}
