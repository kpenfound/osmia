package executionjourney

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func routeJudge(p *jevfixture.Provider) *jev.Judge {
	settings := config.Jev{URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "3s", CheckThreshold: config.DefaultJevCheckThreshold, DecisionThreshold: config.DefaultJevDecisionThreshold}
	return &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: time.Now}
}

func picks(option string, confidence float64) *jevfixture.Provider {
	return &jevfixture.Provider{Confidence: confidence, Pick: func([]string) string { return option }}
}

func (d *draftFixture) withRouteJev(p *jevfixture.Provider) *jev.Judge {
	j := routeJudge(p)
	d.f.s.jev = j
	return j
}

func (d *draftFixture) routeGate(t *testing.T) *intake.Gate {
	t.Helper()
	g, err := d.f.s.delivery().DeliveryRouteGate(context.Background(), d.repository, d.stream)
	fixturekit.Must(t, err)
	return g
}

func (d *draftFixture) runGate(t *testing.T, j *jev.Judge) bool {
	t.Helper()
	ctx := context.Background()
	g := d.routeGate(t)
	if g == nil {
		return false
	}
	decision, err := g.Decision()
	fixturekit.Must(t, err)
	res, err := j.Lookup(d.repository, g.Stream, g.Kind, g.Key)
	fixturekit.Must(t, err)
	switch {
	case decision.Recorded, res.Status == jev.Nonbinding, res.Status == jev.Binding && res.Refused != "", res.Status == jev.FallbackOnly && res.Recorded:
		return true
	}
	if res.Status != jev.Binding {
		state, sources, err := g.State(ctx)
		fixturekit.Must(t, err)
		res = j.Decide(ctx, d.repository, jev.DecisionRequest{Scope: coreadapter.Scope{Project: string(g.Project), Workstream: string(g.Stream)}, Cause: g.Cause, Kind: g.Kind, Occurrence: g.Key, State: state, Options: g.Options, Sources: sources})
		if res.Status != jev.Binding {
			return true
		}
	}
	if apiErr := g.Apply(ctx, res); apiErr != nil {
		if apiErr.Code != intake.Conflict {
			t.Fatalf("applying the pick: %+v", apiErr)
		}
		fixturekit.Must(t, j.Refuse(ctx, d.repository, g.Stream, res.Judgment, apiErr.Message))
	}
	return true
}

func (d *draftFixture) routeResult(t *testing.T) jev.Result {
	t.Helper()
	g := d.routeGate(t)
	if g == nil {
		t.Fatal("the delivery has no gate")
	}
	res, err := jev.Recorded(d.repository, g.Stream, g.Kind, g.Key)
	fixturekit.Must(t, err)
	return res
}

func (d *draftFixture) routeRequests(p *jevfixture.Provider) int { return len(p.Decisions()) }

func (d *draftFixture) refusePush(t *testing.T) {
	t.Helper()
	hook := "#!/bin/sh\nwhile read old new ref; do\n  if [ \"$ref\" = refs/heads/main ]; then\n    echo 'remote: error: GH006: Protected branch update failed for refs/heads/main.' >&2\n    exit 1\n  fi\ndone\n"
	fixturekit.Must(t, os.WriteFile(filepath.Join(d.fork, "hooks", "pre-receive"), []byte(hook), 0700))
}

func (d *draftFixture) card(t *testing.T) (api.InboxEntry, bool) {
	t.Helper()
	entry, found, err := d.f.s.delivery().DeliveryEntry(context.Background(), d.repository, d.stream)
	fixturekit.Must(t, err)
	return entry, found
}

func TestTheDeliveryRouteStateHoldsWhatTheCardShowsAndNoCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newLandJourney(t)
	fixturekit.Must(t, kb.Store(ctx, d.repository, kb.Map{Version: kb.Version, Entities: []kb.Entity{{ID: "internal.trace", Name: "trace", Aliases: []string{}, Paths: []string{"internal/trace"}, Owners: []string{}, PartOf: []string{}}}}, time.Now(), trace.Actor{Kind: "owner", ID: "owner"}, "fixture"))
	d.withRouteJev(picks("land", 0.95))

	g := d.routeGate(t)
	if g == nil {
		t.Fatal("a clean delivery has no gate")
	}
	if g.Kind != jev.KindDeliveryRoute || len(g.Options) != 2 || g.Options[0].Name != "land" || g.Options[1].Name != "pull-request" {
		t.Fatalf("the gate is %+v", g)
	}
	push := d.pushes(t)[0]
	if g.Key != jev.DeliveryRouteKey(d.stream, push.Review, push.Commit) {
		t.Fatalf("the occurrence key is %q", g.Key)
	}
	state, sources, err := g.State(ctx)
	fixturekit.Must(t, err)
	spec := intakefixture.StreamDocuments(t, d.repository, d.stream, plan.SpecDocument)
	for _, want := range []string{delivery.SpecSummary(spec[len(spec)-1].Content), "Uploads resume.", "Checks passed", "2 files changed", "internal.trace: 2 files", "lines added"} {
		if !strings.Contains(state, want) {
			t.Fatalf("the state lacks %q:\n%s", want, state)
		}
	}
	for _, banned := range []string{"diff --git", "@@", "Resume resumes", "Dedupe skips", "package trace", "resume.go", "dedupe.go", "+++", "---"} {
		if strings.Contains(state, banned) {
			t.Fatalf("the state holds %q, which is code or a diff:\n%s", banned, state)
		}
	}
	if len(sources) == 0 {
		t.Fatal("the request names no records it rests on")
	}
	p := picks("land", 0.95)
	d.withRouteJev(p)
	d.runGate(t, d.f.s.jev)
	for _, r := range p.Decisions() {
		if got := jevfixture.Options(r); strings.Join(got, ",") != "land,pull-request,defer" {
			t.Fatalf("Jev was offered %v, want land, pull-request and defer", got)
		}
	}
}

func TestJevIsNotAskedHowToDeliverUnlessTheDeliveryIsClean(t *testing.T) {
	t.Parallel()
	blocking := map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}
	for _, c := range []struct {
		name  string
		build func(t *testing.T) *draftFixture
	}{
		{"a project that delivers by pull request", func(t *testing.T) *draftFixture {
			d := newLandJourney(t)
			d.f.s.cfg.Project.Delivery = config.DeliveryPullRequest
			return d
		}},
		{"a delivery through a fork", func(t *testing.T) *draftFixture {
			d := newLandJourney(t)
			d.f.s.cfg.Project.Fork = "someone/widgets"
			return d
		}},
		{"blocking findings left to the owner", func(t *testing.T) *draftFixture { return newLandJourney(t, blocking) }},
		{"checks that did not pass", func(t *testing.T) *draftFixture {
			d := newLandJourney(t)
			d.failChecks(t)
			return d
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.build(t)
			p := picks("land", 0.99)
			j := d.withRouteJev(p)
			if d.runGate(t, j) {
				t.Fatal("the delivery has a gate")
			}
			if d.routeRequests(p) != 0 {
				t.Fatalf("Jev was asked %d times", d.routeRequests(p))
			}
			entry, found := d.card(t)
			if !found || entry.Recommendation != "" || entry.Jev != nil || entry.Options[0] != "approve" {
				t.Fatalf("the Mark ready card: %+v %v", entry, found)
			}
			if d.upstreamRev(t, "refs/heads/main") == d.mustPushed(t) || d.pulls.Readies != 0 || d.feature(t) != intake.AssembledState {
				t.Fatal("the delivery was applied")
			}
		})
	}
}

func (d *draftFixture) mustPushed(t *testing.T) string {
	t.Helper()
	pushed, ok := d.forkBranch(t)
	if !ok {
		t.Fatal("the draft's commit was not pushed")
	}
	return pushed
}

func TestAConfidentLandPickLandsTheWorkstreamUnderJevsAuthority(t *testing.T) {
	t.Parallel()
	d := newLandJourney(t)
	p := picks("land", 0.95)
	j := d.withRouteJev(p)
	pushed := d.mustPushed(t)
	base := d.upstreamRev(t, "refs/heads/main")

	if !d.runGate(t, j) {
		t.Fatal("the delivery has no gate")
	}
	if d.routeRequests(p) != 1 {
		t.Fatalf("Jev was asked %d times", d.routeRequests(p))
	}
	if got := d.upstreamRev(t, "refs/heads/main"); got != pushed {
		t.Fatalf("the base branch is at %s, want the pushed commit %s (was %s)", got, pushed, base)
	}
	res := d.workstreamJudgment(t)
	landing, found, err := delivery.ReadDeliveryLanding(d.repository, d.stream)
	fixturekit.Must(t, err)
	if !found || landing.DecidedBy != trace.DecidedByJev || landing.Judgment != res.Judgment || landing.Commit != pushed {
		t.Fatalf("the landing record is %+v, want Jev's judgment %s", landing, res.Judgment)
	}
	if d.feature(t) != intake.DeliveredState || d.pulls.Readies != 0 || d.approvals(t) != 0 {
		t.Fatalf("the workstream is %s, %d ready, %d approvals", d.feature(t), d.pulls.Readies, d.approvals(t))
	}
	if _, open := d.card(t); open {
		t.Fatal("an owner card is open")
	}
	if res.Refused != "" {
		t.Fatalf("the pick is recorded refused: %s", res.Refused)
	}

	d.runGate(t, j)
	if d.routeRequests(p) != 1 || fixturekitLandings(t, d) != 1 {
		t.Fatalf("%d requests and %d landings after a second look", d.routeRequests(p), fixturekitLandings(t, d))
	}
}

func (d *draftFixture) workstreamJudgment(t *testing.T) jev.Result {
	t.Helper()
	push := d.pushes(t)[0]
	res, err := jev.Recorded(d.repository, d.stream, jev.KindDeliveryRoute, jev.DeliveryRouteKey(d.stream, push.Review, push.Commit))
	fixturekit.Must(t, err)
	if res.Status != jev.Binding {
		t.Fatalf("the recorded result is %+v", res)
	}
	return res
}

func TestAConfidentPullRequestPickMarksTheDraftReady(t *testing.T) {
	t.Parallel()
	d := newLandJourney(t)
	p := picks("pull-request", 0.97)
	j := d.withRouteJev(p)
	base := d.upstreamRev(t, "refs/heads/main")

	d.runGate(t, j)
	res := d.workstreamJudgment(t)
	_, decision, approval, err := delivery.DeliveryDocuments(d.repository, d.stream)
	fixturekit.Must(t, err)
	if approval == nil || approval.DecidedBy != trace.DecidedByJev || approval.Judgment != res.Judgment || approval.Route != "" || decision.Revision != 1 {
		t.Fatalf("the approval is %+v", approval)
	}
	if _, open := d.card(t); open {
		t.Fatal("an owner card is open")
	}
	d.settleReady(t)
	if d.pulls.Readies != 1 || d.pulls.Prs[0].Draft || len(d.pulls.Prs) != 1 || d.pulls.Creates != 1 {
		t.Fatalf("pull requests %+v, %d ready, %d created", d.pulls.Prs, d.pulls.Readies, d.pulls.Creates)
	}
	if d.feature(t) != intake.DeliveredState || d.upstreamRev(t, "refs/heads/main") != base || fixturekitLandings(t, d) != 0 {
		t.Fatalf("the workstream is %s; base at %s, was %s; %d landings", d.feature(t), d.upstreamRev(t, "refs/heads/main"), base, fixturekitLandings(t, d))
	}
	d.runGate(t, j)
	if d.routeRequests(p) != 1 || d.approvals(t) != 1 {
		t.Fatalf("%d requests and %d approvals after a second look", d.routeRequests(p), d.approvals(t))
	}
}

func TestAnUnconfidentDeferringOrFailingJevLeavesTheDeliveryToTheOwner(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		provider  *jevfixture.Provider
		recommend string
	}{
		{"below the threshold", picks("land", 0.6), "land"},
		{"below the threshold for the pull request", picks("pull-request", 0.6), "pull-request"},
		{"a deferral", picks("defer", 0.99), ""},
		{"a failing Jev", &jevfixture.Provider{Confidence: 0.99, Fail: func(int) error { return context.DeadlineExceeded }}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newLandJourney(t)
			j := d.withRouteJev(c.provider)
			base := d.upstreamRev(t, "refs/heads/main")
			d.runGate(t, j)
			if d.upstreamRev(t, "refs/heads/main") != base || d.pulls.Readies != 0 || d.feature(t) != intake.AssembledState || d.approvals(t) != 0 {
				t.Fatal("Jev's answer was applied")
			}
			entry, found := d.card(t)
			if !found || strings.Join(entry.Options, ",") != "approve,land" || entry.Land == nil {
				t.Fatalf("the card does not offer both routes: %+v %v", entry, found)
			}
			if got := d.present(t).Recommendation; got != c.recommend {
				t.Fatalf("the recommendation is %q, want %q", got, c.recommend)
			}
			if (c.recommend != "") != strings.Contains(entry.Recommendation, "Jev recommends") {
				t.Fatalf("the card's recommendation is %q for the pick %q", entry.Recommendation, c.recommend)
			}
		})
	}
}

func TestAConfidentLandPickTheUpstreamRefusesTakesTheFallback(t *testing.T) {
	t.Parallel()
	d := newLandJourney(t)
	p := picks("land", 0.95)
	j := d.withRouteJev(p)
	d.refusePush(t)
	base := d.upstreamRev(t, "refs/heads/main")

	d.runGate(t, j)
	if got := d.upstreamRev(t, "refs/heads/main"); got != base {
		t.Fatalf("the refused push moved the base branch to %s", got)
	}
	if d.pulls.Readies != 1 || d.pulls.Prs[0].Draft || len(d.pulls.Prs) != 1 || d.pulls.Creates != 1 || d.feature(t) != intake.DeliveredState {
		t.Fatalf("pull requests %+v, %d ready, %d created, workstream %s", d.pulls.Prs, d.pulls.Readies, d.pulls.Creates, d.feature(t))
	}
	if _, open := d.card(t); open {
		t.Fatal("an owner card is open")
	}
	if res := d.workstreamJudgment(t); res.Refused != "" {
		t.Fatalf("the pick is recorded refused: %s", res.Refused)
	}
	notices, err := d.repository.Outbox(d.stream)
	fixturekit.Must(t, err)
	said := false
	for _, n := range notices {
		said = said || strings.Contains(n.Event.Body, "delivered without landing") && strings.Contains(n.Event.Body, "Protected branch")
	}
	if !said {
		t.Fatal("no notice says why the workstream did not land")
	}
}

func (d *draftFixture) busy(t *testing.T) {
	t.Helper()
	event := trace.EventID("fixture-busy", "run")
	op := coreadapter.Operation{ID: trace.OperationID(d.repository.Project(), d.stream, event), Boundary: coreadapter.RepositoryBoundary, Action: delivery.FinalReviewAction, Input: json.RawMessage(`{}`)}
	state, err := d.repository.Workflow(d.stream, "fixture-busy")
	fixturekit.Must(t, err)
	_, err = d.repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-busy", Revision: 1, Project: d.repository.Project(), Workstream: d.stream, At: time.Now(), Actor: service.TestForemanActor, Cause: "fixture"}, Subject: "fixture-busy", From: state.Value, To: "requested", Reason: "fixture"},
		Events:     []trace.Event{{ID: event, Kind: delivery.FinalReviewAction, Body: "a final review", Operation: &op}}})
	fixturekit.Must(t, err)
}

func (d *draftFixture) finishBusy(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	event := trace.EventID("fixture-busy", "run")
	a, _, err := d.repository.BeginOperation(ctx, d.stream, event, trace.WorkerOperation, service.TestForemanActor, time.Now)
	fixturekit.Must(t, err)
	if a == nil {
		t.Fatal("the busy operation cannot be claimed")
	}
	defer a.Close()
	for _, step := range []string{"observe", "effect", "result"} {
		action := a.Action(step, time.Now())
		switch step {
		case "observe":
			action.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "nothing yet"}
		case "result":
			action.Result = &coreadapter.OperationResult{Outcome: "succeeded", Evidence: "done"}
		}
		fixturekit.Must(t, a.Record(ctx, action))
	}
}

func TestAConfidentLandPickTheRouterHoldsIsAppliedWhenTheHoldClears(t *testing.T) {
	t.Parallel()
	d := newLandJourney(t)
	p := picks("land", 0.95)
	j := d.withRouteJev(p)
	pushed := d.mustPushed(t)
	base := d.upstreamRev(t, "refs/heads/main")
	d.busy(t)

	d.runGate(t, j)
	if got := d.upstreamRev(t, "refs/heads/main"); got != base {
		t.Fatalf("a held landing pushed: the base branch is at %s", got)
	}
	if _, open := d.card(t); open || d.feature(t) != intake.AssembledState || d.pulls.Readies != 0 {
		t.Fatalf("a held landing raised a card or changed the workstream: open %v, %s, %d ready", open, d.feature(t), d.pulls.Readies)
	}
	if res := d.workstreamJudgment(t); res.Refused != "" {
		t.Fatalf("a hold is recorded as a refusal: %s", res.Refused)
	}

	d.runGate(t, j)
	if d.routeRequests(p) != 1 || d.upstreamRev(t, "refs/heads/main") != base {
		t.Fatal("a held pick was asked or applied again")
	}

	d.finishBusy(t)
	records, err := d.repository.Operations(d.stream)
	fixturekit.Must(t, err)
	for _, r := range records {
		if delivery.LandingAction(r.Operation) {
			lander := &delivery.Lander{S: d.f.s.delivery(), Repository: d.repository}
			if result := settleOperation(t, d.f.s, d.repository, d.stream, r.Operation, lander); result.Outcome != api.LandOutcomeLanded {
				t.Fatalf("the landing once the hold cleared: %+v", result)
			}
		}
	}
	if got := d.upstreamRev(t, "refs/heads/main"); got != pushed || d.feature(t) != intake.DeliveredState {
		t.Fatalf("the base branch is at %s, want %s; the workstream is %s", got, pushed, d.feature(t))
	}
	res := d.workstreamJudgment(t)
	landing, found, err := delivery.ReadDeliveryLanding(d.repository, d.stream)
	fixturekit.Must(t, err)
	if !found || landing.DecidedBy != trace.DecidedByJev || landing.Judgment != res.Judgment {
		t.Fatalf("the landing record is %+v", landing)
	}
}

func TestABindingPickTheServiceRefusesToApplyGoesToTheOwnerWithJevsJudgment(t *testing.T) {
	t.Parallel()
	d := newLandJourney(t)
	p := picks("land", 0.95)
	j := d.withRouteJev(p)
	ctx := context.Background()
	base := d.upstreamRev(t, "refs/heads/main")

	g := d.routeGate(t)
	state, sources, err := g.State(ctx)
	fixturekit.Must(t, err)
	res := j.Decide(ctx, d.repository, jev.DecisionRequest{Scope: coreadapter.Scope{Project: string(g.Project), Workstream: string(g.Stream)}, Kind: g.Kind, Occurrence: g.Key, State: state, Options: g.Options, Sources: sources})
	if res.Status != jev.Binding || res.Pick != "land" {
		t.Fatalf("the judgment is %+v", res)
	}
	d.failChecks(t)
	d.runGate(t, j)

	if d.upstreamRev(t, "refs/heads/main") != base || d.feature(t) != intake.AssembledState || fixturekitLandings(t, d) != 0 {
		t.Fatal("a refused pick pushed or recorded a landing")
	}
	recorded := d.routeResult(t)
	if recorded.Status != jev.Binding || !strings.Contains(recorded.Refused, "did not pass") {
		t.Fatalf("the recorded result is %+v, want a refusal naming the checks", recorded)
	}
	if got := d.present(t).Recommendation; got != "land" {
		t.Fatalf("the presentation's recommendation is %q", got)
	}
	entry, found := d.card(t)
	if !found || strings.Join(entry.Options, ",") != "approve,land" {
		t.Fatalf("the owner's card: %+v %v", entry, found)
	}

	d.runGate(t, j)
	if d.routeRequests(p) != 1 || d.upstreamRev(t, "refs/heads/main") != base {
		t.Fatalf("a refused pick was asked %d times or applied", d.routeRequests(p))
	}
}
