package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordSealed(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, spec string, units []plan.Unit) {
	t.Helper()
	encoded, err := plan.Encode(plan.Plan{Version: plan.Version, Units: units})
	fixturekit.Must(t, err)
	sealed, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(spec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: strings.Repeat("a", 40)}, Branch: "osmia/" + string(stream), Workspace: "/nonexistent/" + string(stream)})
	fixturekit.Must(t, err)
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: intake.OwnerActor, Cause: "fixture"}
	}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{
		{Header: header(plan.SpecDocument), Path: plan.SpecPath, Content: spec},
		{Header: header(plan.PlanDocument), Path: plan.PlanPath, Content: string(encoded)},
		{Header: header(seal.DocumentID), Path: seal.Path, Content: string(sealed)},
	}))
}

func recordFinalReport(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, report api.FinalReport) {
	t.Helper()
	existing, err := delivery.FinalReports(repo, stream)
	fixturekit.Must(t, err)
	data, err := json.Marshal(report)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: len(existing) + 1, Project: repo.Project(), Workstream: stream, At: at, Actor: intake.OwnerActor, Cause: "fixture"}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: delivery.FinalReportPath, Content: string(data) + "\n"}}))
}

func requestText(t *testing.T, r systemone.Request) string {
	t.Helper()
	state, err := json.Marshal(r.State)
	fixturekit.Must(t, err)
	var summary string
	if json.Unmarshal(state, &summary) == nil {
		state = []byte(summary)
	}
	return string(state) + "\n" + intakefixture.RequestJSON(t, r)
}

func requireIn(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s lacks %q", what, want)
		}
	}
}

func requireNotIn(t *testing.T, what, text string, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(text, bad) {
			t.Errorf("%s carries %q", what, bad)
		}
	}
}

const genericIntent = "dropping a dependency must not change what the owner approved"

func unitOf(id, title, task string, criteria ...string) plan.Unit {
	return plan.Unit{ID: id, Title: title, Task: task, Acceptance: []string{"The " + id + " unit works."}, Criteria: criteria}
}

func TestADependencyExitCarriesTheApprovedIntentForJev(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	at := f.s.Now()
	recordSealed(t, f.repository, f.downstream, at,
		"# Dependent\n\n## Intent\n\nDEPENDENT-INTENT keeps uploads resumable.\n\n```go\nfunc codeInIntent() {}\n```\n\n## Intended behaviour\n\nNOT-THE-INTENT\n\n## Acceptance criteria\n\n1. OUTCOME-ONE resumes uploads.\n2. OUTCOME-TWO speaks the widget protocol.\n",
		[]plan.Unit{
			unitOf("client", "Client resumes", "Resume against the endpoint of workstream "+string(f.upstream), "spec#1"),
			unitOf("wire", "Wire protocol", "Speak the protocol", "spec#2"),
			unitOf("docs", "Docs", "Write the documentation", "spec#1"),
		})
	recordSealed(t, f.repository, f.upstream, at, "# Upstream\n\n## Intent\n\nServe chunk uploads.\n\n## Acceptance criteria\n\n1. Chunks are stored.\n",
		[]plan.Unit{unitOf("endpoint", "Upstream endpoint", "Serve chunk uploads", "spec#1"), unitOf("auth", "Upstream auth", "Authenticate the clients", "spec#1")})
	recordFinalReport(t, f.repository, f.upstream, at, api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed, Summary: "UPSTREAM-SUMMARY says the endpoint is unfinished",
		Findings: []api.FinalFinding{{Severity: delivery.FindingBlocking, Finding: "The endpoint is unfinished."}}})
	groups, _, err := trace.Workgroups(f.repository)
	fixturekit.Must(t, err)
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, f.repository, groups[0], trace.ProjectSpec{
		Behavior:  "Downstream depends on upstream.",
		Contracts: []trace.Contract{{Name: "widget-protocol", Text: "WIDGET-CONTRACT the upstream speaks widgets."}},
		Graph:     []trace.Edge{{From: f.upstream, To: f.downstream, Type: trace.EdgeHardBlock}},
		Slices:    []trace.Slice{{Workstream: f.downstream, ContractCriteria: []int{2}}},
	}, at, intake.OwnerActor, "fixture"))

	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.Now(), f.upstream)
	f.resolvePasses(t)

	requests := p.TransitionRequests()
	if len(requests) != 1 {
		t.Fatalf("Jev got %d transition requests, want 1", len(requests))
	}
	text := requestText(t, requests[0])
	requireIn(t, "the request", text,
		"DEPENDENT-INTENT", "OUTCOME-ONE", "OUTCOME-TWO", "spec#2",
		"WIDGET-CONTRACT", "widget-protocol", "contract sections and edges of the dependent",
		"unit client", "unit wire", `"relying_units":2`, `"dependent_plan_units":3`,
		"Upstream endpoint", "Upstream auth", `"upstream_planned_units":2`, `"dependent_acceptance_outcomes":2`,
		"UPSTREAM-SUMMARY", "abandoned for the test", "its latest final report is review 1",
	)
	requireNotIn(t, "the request", text, genericIntent, "NOT-THE-INTENT", "codeInIntent", "unit docs (Docs)", "[cut:")
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal); len(records) != 1 || records[0].State != router.StateCommitted {
		t.Fatalf("route %+v", records)
	}
}

func TestADependencyExitWhoseUpstreamHasNoPlanOrOutcomeStillRoutes(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	recordSealed(t, f.repository, f.downstream, f.s.Now(), "# Dependent\n\n## Intent\n\nDEPENDENT-INTENT.\n\n## Acceptance criteria\n\n1. OUTCOME-ONE.\n",
		[]plan.Unit{unitOf("client", "Client", "Build the client", "spec#1")})
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.Now(), f.upstream)
	f.resolvePasses(t)

	requests := p.TransitionRequests()
	if len(requests) != 1 {
		t.Fatalf("Jev got %d transition requests, want 1", len(requests))
	}
	text := requestText(t, requests[0])
	requireIn(t, "the request", text, "DEPENDENT-INTENT", "missing from the records", "the upstream has no plan", "no final report is on record", "no merge of its branch is on record", "contracts")
	requireNotIn(t, "the request", text, genericIntent)
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Outcome != router.OutcomeFollowed {
		t.Fatalf("the exit did not route: %+v", records)
	}
}

func TestADependencyExitOfADependentWithoutASealedSpecStillRoutes(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.Now(), f.upstream)
	f.resolvePasses(t)

	requests := p.TransitionRequests()
	if len(requests) != 1 {
		t.Fatalf("Jev got %d transition requests, want 1", len(requests))
	}
	text := requestText(t, requests[0])
	requireIn(t, "the request", text, "has no sealed spec or plan", "missing from the records")
	requireNotIn(t, "the request", text, genericIntent)
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal); len(records) != 1 || records[0].State != router.StateCommitted {
		t.Fatalf("the exit did not route: %+v", records)
	}
}

func TestAnIntentChangingDropOfADependencyGoesToTheOwnerAndNothingIsDropped(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	recordSealed(t, f.repository, f.downstream, f.s.Now(), "# Dependent\n\n## Intent\n\nDEPENDENT-INTENT.\n\n## Acceptance criteria\n\n1. OUTCOME-ONE.\n",
		[]plan.Unit{unitOf("client", "Client", "Build the client", "spec#1")})
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger != lifecycle.TriggerUpstreamTerminal {
			return jevfixture.DefaultFits(a)
		}
		reply := jevfixture.Departs(a, lifecycle.DependencyWaived, 0.95, 0.95)
		reply.Intent = 0.99
		return reply
	}}
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
	hooks := fixturekit.NewHooks(t)
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.Now(), f.upstream)
	f.resolvePasses(t)
	f.resolvePasses(t)

	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
	requireIn(t, "the request", requestText(t, p.TransitionRequests()[0]), "DEPENDENT-INTENT", "OUTCOME-ONE")
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].State != router.StateOwner || records[0].Hold != router.HoldIntent || records[0].Outcome != router.OutcomeHeld || records[0].Target != lifecycle.DependencyWaived {
		t.Fatalf("route %+v", records)
	}
	if waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream); err != nil || len(waivers.Waivers) != 0 {
		t.Fatalf("the move to the owner wrote a waiver: %+v %v", waivers, err)
	}
	if state, err := f.repository.Workflow(f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock)); err != nil || state.Version != 0 {
		t.Fatalf("the move to the owner moved the decision: %+v %v", state, err)
	}
	if state := f.waitState(t); state.Value != "waiting" {
		t.Fatalf("the edge wait is %+v", state)
	}
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent was released")
	}

	f.s.SetSole(journey.RuntimeFor(f.repository))
	feed, apiErr := service.InspectForTest(f.s.Core()).Feed(string(f.downstream))
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	cards := 0
	for _, e := range feed.Entries {
		if e.Recovery != nil {
			cards++
			if e.Recovery.Target != lifecycle.DependencyWaived || e.Recovery.Machine != lifecycle.DependencyMachine || e.Recovery.Origin != api.RecoveryIntent {
				t.Fatalf("the recovery card %+v", e.Recovery)
			}
		}
	}
	if cards != 1 {
		t.Fatalf("%d recovery cards, want 1", cards)
	}
	for _, d := range f.publish(t, hooks) {
		if strings.Contains(d.Raw, "waiver") || strings.Contains(d.Raw, string(f.upstream)) {
			t.Fatalf("a move that belongs to the owner reached Hearsay: %s", d.Raw)
		}
	}
}

func TestIntentEvidenceOfLargeRecordsStaysBoundedAndSaysWhatWasCut(t *testing.T) {
	t.Parallel()
	const code = "CODE-MARKER-FUNC"
	long := func(word string, n int) string { return strings.Repeat(word+" ", n) }

	t.Run("dependency exit", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
		at := f.s.Now()
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
		fixturekit.Must(t, err)
		fixturekit.Must(t, trace.RecordProjectSpec(ctx, f.repository, groups[0], trace.ProjectSpec{Behavior: "Large.", Contracts: contracts,
			Graph: []trace.Edge{{From: f.upstream, To: f.downstream, Type: trace.EdgeHardBlock}}}, at, intake.OwnerActor, "fixture"))

		p := &jevfixture.Transitions{}
		journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
		f.resolvePasses(t)
		endStream(t, f.repository, f.s.Now(), f.upstream)
		f.resolvePasses(t)

		requests := p.TransitionRequests()
		if len(requests) != 1 {
			t.Fatalf("Jev got %d transition requests, want 1", len(requests))
		}
		text := requestText(t, requests[0])
		if n := len(intakefixture.RequestJSON(t, requests[0])); n > jev.MaxDecisionBytes {
			t.Fatalf("the request is %d bytes, over %d", n, jev.MaxDecisionBytes)
		}
		requireIn(t, "the request", text, "[cut:", `"dependent_acceptance_outcomes":300`, `"contracts":60`, `"dependent_plan_units":150`, `"upstream_planned_units":150`, "[code omitted]", "shortened")
		requireNotIn(t, "the request", text, code, genericIntent)
		if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal); len(records) != 1 || records[0].Outcome != router.OutcomeFollowed {
			t.Fatalf("the request was too large to answer: %+v", records)
		}
	})
}
