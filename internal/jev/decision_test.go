package jev_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// judgmentRecords returns every revision of every judgment record.
func (f *fixture) judgmentRecords(t *testing.T) []jev.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, streamID)
	if err != nil {
		t.Fatal(err)
	}
	var out []jev.Record
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "judgments/") {
			continue
		}
		var r jev.Record
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func decisionScope() coreadapter.Scope {
	return coreadapter.Scope{Project: string(projectID), Workstream: string(streamID)}
}

func decisionRequest(kind jev.DecisionKind, occurrence, state string, options ...string) jev.DecisionRequest {
	req := jev.DecisionRequest{Scope: decisionScope(), Cause: "request_1", Kind: kind, Occurrence: occurrence, State: state}
	for _, name := range options {
		req.Options = append(req.Options, jev.DecisionOption{Name: name, Description: name + " it"})
	}
	return req
}

func pick(choice string, confidence float64, others ...string) jevtest.Result {
	return jevtest.Result{Response: systemone.Response{
		Model:   "jev-1.13.0",
		Answers: map[string]systemone.Answer{"decision": jevtest.Choice(choice, confidence, confidence, others...)},
		Usage:   systemone.Usage{InputTokens: 100, OutputTokens: 5, CostUSD: 0.000004, CostKnown: true},
	}}
}

func TestDecisionPickAtTheThresholdIsBindingAndBelowItIsNonbinding(t *testing.T) {
	for _, c := range []struct {
		name       string
		confidence float64
		want       jev.ResultStatus
	}{{"above", 0.95, jev.Binding}, {"at", 0.9, jev.Binding}, {"below", 0.89, jev.Nonbinding}} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			settings := testSettings()
			settings.DecisionThreshold = 0.9
			p := &jevtest.Provider{Results: []jevtest.Result{pick("answer", c.confidence, "escalate")}}
			res := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindQuestionRouting, jev.QuestionKey("q1"), "state", "answer", "escalate"))
			if res.Status != c.want || res.Pick != "answer" || res.Confidence != c.confidence || res.Threshold != 0.9 || res.Judgment == "" || res.Recorded {
				t.Fatalf("result = %+v", res)
			}
			if len(p.Requests()) != 1 {
				t.Fatalf("requests = %d", len(p.Requests()))
			}
		})
	}
}

func TestDecisionKindsHaveDistinctTasksVersionsAndRecordTheThresholdAsAcceptance(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	settings.DecisionThreshold = 0.8
	settings.OwnerThreshold = 0.95
	p := &jevtest.Provider{Results: []jevtest.Result{pick("a", 0.99, "b")}}
	j := judge(&settings, p, "key")
	tasks := map[string]bool{}
	owner := map[string]bool{}
	for _, kind := range jev.Kinds() {
		if kind.Version() < 1 {
			t.Fatalf("%s has version %d", kind, kind.Version())
		}
		tasks[kind.Task()] = true
		owner[kind.Task()] = kind.Owner()
		res := j.Decide(context.Background(), f.repo, decisionRequest(kind, "subject:1", "state", "a", "b"))
		if res.Status != jev.Binding {
			t.Fatalf("%s: %+v", kind, res)
		}
	}
	if len(tasks) != 18 || len(jev.Kinds()) != 18 {
		t.Fatalf("tasks = %v", tasks)
	}
	records := f.judgmentRecords(t)
	seen := map[string]bool{}
	for _, r := range records {
		if r.State != "accepted" {
			continue
		}
		seen[r.Task] = true
		acceptance, threshold := "decision_threshold=0.8", 0.8
		if owner[r.Task] {
			acceptance, threshold = "owner_threshold=0.95", 0.95
		}
		if r.Acceptance != acceptance || r.Occurrence == nil || r.Occurrence.Threshold != threshold || r.Occurrence.Key != "subject:1" || r.Version != 1 {
			t.Fatalf("record = %+v", r)
		}
	}
	if len(seen) != 18 {
		t.Fatalf("recorded tasks = %v", seen)
	}
}

// The owner's own decisions are binding at the owner threshold, independent of
// the decision threshold, and the other kinds keep the decision threshold.
func TestTheOwnersOwnDecisionsAreBindingAtTheOwnerThreshold(t *testing.T) {
	owners := []jev.DecisionKind{jev.KindDeliveryApproval, jev.KindCharterProposal, jev.KindCharterConflict, jev.KindArchitectEscalation, jev.KindOwnerObjection, jev.KindCancellation}
	for _, kind := range jev.Kinds() {
		want := false
		for _, o := range owners {
			want = want || o == kind
		}
		if kind.Owner() != want {
			t.Fatalf("%s owner = %v", kind, kind.Owner())
		}
	}
	for _, c := range []struct {
		kind       jev.DecisionKind
		confidence float64
		want       jev.ResultStatus
	}{
		{jev.KindDeliveryApproval, 0.92, jev.Nonbinding},
		{jev.KindDeliveryApproval, 0.96, jev.Binding},
		{jev.KindRatification, 0.92, jev.Binding},
		{jev.KindRatification, 0.89, jev.Nonbinding},
	} {
		f := newFixture(t)
		settings := testSettings()
		settings.DecisionThreshold, settings.OwnerThreshold = 0.9, 0.95
		p := &jevtest.Provider{Results: []jevtest.Result{pick("approve", c.confidence, "other")}}
		res := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(c.kind, "subject:1", "state", "approve", "other"))
		if res.Status != c.want {
			t.Fatalf("%s at %.2f: %+v", c.kind, c.confidence, res)
		}
	}
}

func TestDecisionKindsHaveTheirOwnJudgmentForTheSameKey(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("a", 0.99, "b")}}
	j := judge(&settings, p, "key")
	j.Decide(context.Background(), f.repo, decisionRequest(jev.KindObjection, "same", "state", "a", "b"))
	res := j.Decide(context.Background(), f.repo, decisionRequest(jev.KindContestedRuling, "same", "state", "a", "b"))
	if res.Recorded || len(p.Requests()) != 2 {
		t.Fatalf("result = %+v, requests = %d", res, len(p.Requests()))
	}
}

func TestABindingPickIsReturnedWithoutARequestWhateverChangedAndAfterARestart(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("review", 0.97, "revise", "escalate")}}
	first := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindContestedRuling, jev.ContestKey("contest_1"), "state", "review", "revise", "escalate"))
	if first.Status != jev.Binding || first.Pick != "review" {
		t.Fatalf("first = %+v", first)
	}

	later := &jevtest.Provider{Results: []jevtest.Result{pick("escalate", 0.99, "review", "revise")}}
	settings.Model = "jev-1.14.0"
	settings.DecisionThreshold = 0.99
	changed := decisionRequest(jev.KindContestedRuling, jev.ContestKey("contest_1"), "a different summary of the evidence", "review", "escalate")
	check := func(res jev.Result) {
		t.Helper()
		if res.Status != jev.Binding || res.Pick != "review" || res.Confidence != 0.97 || res.Threshold != 0.9 || res.Judgment != first.Judgment || !res.Recorded {
			t.Fatalf("result = %+v, first = %+v", res, first)
		}
	}
	check(judge(&settings, later, "key").Decide(context.Background(), f.repo, changed))
	f.reopen(t)
	check(judge(&settings, later, "key").Decide(context.Background(), f.repo, changed))
	// An unconfigured Jev still returns it.
	check(judge(&settings, later, "").Decide(context.Background(), f.repo, changed))
	if len(later.Requests()) != 0 {
		t.Fatalf("requests = %d", len(later.Requests()))
	}
	if got := len(f.judgmentRecords(t)); got != 2 {
		t.Fatalf("judgment revisions = %d", got)
	}
}

func TestABelowThresholdAnswerIsTheOccurrencesNonbindingResult(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("dismiss", 0.6, "escalate")}}
	j := judge(&settings, p, "key")
	first := j.Decide(context.Background(), f.repo, decisionRequest(jev.KindObjection, jev.ObjectionKey("o1"), "state", "dismiss", "escalate"))
	if first.Status != jev.Nonbinding {
		t.Fatalf("first = %+v", first)
	}
	again := judge(&settings, &jevtest.Provider{Results: []jevtest.Result{pick("escalate", 0.99, "dismiss")}}, "key")
	f.reopen(t)
	second := again.Decide(context.Background(), f.repo, decisionRequest(jev.KindObjection, jev.ObjectionKey("o1"), "other state", "dismiss", "escalate"))
	if second.Status != jev.Nonbinding || second.Pick != "dismiss" || second.Confidence != 0.6 || !second.Recorded || second.Judgment != first.Judgment {
		t.Fatalf("second = %+v", second)
	}
	if len(p.Requests()) != 1 {
		t.Fatalf("requests = %d", len(p.Requests()))
	}
}

func TestAFallbackIsNeverAPickAndALaterAskSendsANewRequest(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	off := &jevtest.Provider{Results: []jevtest.Result{pick("approve", 0.99, "decline")}}
	fell := judge(&settings, off, "").Decide(context.Background(), f.repo, decisionRequest(jev.KindPlanAmendment, jev.AmendmentKey("a1"), "state", "approve", "decline"))
	if fell.Status != jev.FallbackOnly || fell.Reason != jev.ReasonUnconfigured || fell.Pick != "" || len(off.Requests()) != 0 {
		t.Fatalf("fallback = %+v", fell)
	}
	lookup, err := judge(&settings, off, "key").Lookup(f.repo, streamID, jev.KindPlanAmendment, jev.AmendmentKey("a1"))
	if err != nil || lookup.Status != jev.FallbackOnly || lookup.Reason != jev.ReasonUnconfigured || lookup.Pick != "" {
		t.Fatalf("lookup = %+v %v", lookup, err)
	}

	f.reopen(t)
	on := &jevtest.Provider{Results: []jevtest.Result{pick("approve", 0.95, "decline")}}
	res := judge(&settings, on, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindPlanAmendment, jev.AmendmentKey("a1"), "state", "approve", "decline"))
	if res.Status != jev.Binding || res.Pick != "approve" || res.Recorded || len(on.Requests()) != 1 {
		t.Fatalf("result = %+v, requests = %d", res, len(on.Requests()))
	}
	if res.Judgment == fell.Judgment {
		t.Fatal("the new request reused the fallback's record")
	}
}

func TestEachFailedAskIsRecordedAndTheNextOneAsksAgain(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{
		{Err: &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"}},
		{Err: &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"}},
		{Err: &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"}},
		{Err: &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"}},
		pick("approve", 0.95, "decline"),
	}}
	j := judge(&settings, p, "key")
	req := decisionRequest(jev.KindPlanAmendment, jev.AmendmentKey("a1"), "state", "approve", "decline")
	first := j.Decide(context.Background(), f.repo, req)
	second := j.Decide(context.Background(), f.repo, req)
	if first.Status != jev.FallbackOnly || first.Reason != jev.ReasonUnavailable || second.Status != jev.FallbackOnly || first.Judgment == second.Judgment {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
}

func TestAnOversizedRequestIsARecordedFallbackWithoutARequest(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("a", 0.99, "b")}}
	res := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindTriage, jev.FindingKey(1, "f1"), strings.Repeat("x", jev.MaxDecisionBytes), "a", "b"))
	if res.Status != jev.FallbackOnly || res.Reason != jev.ReasonRejected || len(p.Requests()) != 0 {
		t.Fatalf("result = %+v, requests = %d", res, len(p.Requests()))
	}
	lookup, err := judge(&settings, p, "key").Lookup(f.repo, streamID, jev.KindTriage, jev.FindingKey(1, "f1"))
	if err != nil || lookup.Status != jev.FallbackOnly || lookup.Reason != jev.ReasonRejected {
		t.Fatalf("lookup = %+v %v", lookup, err)
	}
}

func TestACoolingProviderIsAFallbackForTheDecision(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindRateLimited, RetryAfter: time.Minute}}}}
	j := judge(&settings, p, "key")
	j.Decide(context.Background(), f.repo, decisionRequest(jev.KindDiscovery, jev.DiscoveryKey("d1"), "state", "assign", "decline"))
	res := j.Decide(context.Background(), f.repo, decisionRequest(jev.KindDiscovery, jev.DiscoveryKey("d2"), "state", "assign", "decline"))
	if res.Status != jev.FallbackOnly || res.Reason != jev.ReasonCoolingDown || len(p.Requests()) != 1 {
		t.Fatalf("result = %+v, requests = %d", res, len(p.Requests()))
	}
}

func TestAPickOutsideTheOptionsIsAnUnusableFallback(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("elsewhere", 0.99, "a")}}
	res := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindObjection, jev.ObjectionKey("o1"), "state", "a", "b"))
	if res.Status != jev.FallbackOnly || res.Reason != jev.ReasonDeclined || res.Pick != "" {
		t.Fatalf("result = %+v", res)
	}
}

func TestASingleOptionDecisionOffersDeferAndAPickOfItIsNeverBinding(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("ratify", 0.97, "defer")}}
	res := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindRatification, jev.RatificationKey(1, 2, 3), "packet", "ratify"))
	if res.Status != jev.Binding || res.Pick != "ratify" {
		t.Fatalf("result = %+v", res)
	}
	options := p.Requests()[0].Questions["decision"].Options
	if len(options) != 2 || options[1].Name != jev.Defer {
		t.Fatalf("options = %+v", options)
	}

	deferred := &jevtest.Provider{Results: []jevtest.Result{pick("defer", 0.99, "ratify")}}
	res = judge(&settings, deferred, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindRatification, jev.RatificationKey(1, 4, 3), "packet", "ratify"))
	if res.Status != jev.Nonbinding || !res.Deferred || res.Pick != "" {
		t.Fatalf("deferred = %+v", res)
	}
}

func TestTwoContestsOfTheSameUnitAreJudgedIndependently(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("review", 0.97, "escalate"), pick("escalate", 0.5, "review")}}
	j := judge(&settings, p, "key")
	req := func(contest string) jev.DecisionRequest {
		r := decisionRequest(jev.KindContestedRuling, jev.ContestKey(contest), "unit1 is contested", "review", "escalate")
		r.Scope.Unit = "unit1"
		return r
	}
	first := j.Decide(context.Background(), f.repo, req("contest_1"))
	second := j.Decide(context.Background(), f.repo, req("contest_2"))
	if first.Status != jev.Binding || first.Pick != "review" || second.Status != jev.Nonbinding || second.Pick != "escalate" || len(p.Requests()) != 2 {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
	again := j.Decide(context.Background(), f.repo, req("contest_1"))
	if again.Status != jev.Binding || again.Pick != "review" || !again.Recorded || len(p.Requests()) != 2 {
		t.Fatalf("again = %+v", again)
	}
}

func TestTheLookupReportsEachResultWithoutAskingJev(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("approve", 0.97, "decline"), pick("approve", 0.4, "decline")}}
	j := judge(&settings, p, "key")
	look := func(key string) jev.Result {
		t.Helper()
		res, err := j.Lookup(f.repo, streamID, jev.KindPlanAmendment, key)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := look(jev.AmendmentKey("none")); res.Status != jev.None {
		t.Fatalf("none = %+v", res)
	}
	j.Decide(context.Background(), f.repo, decisionRequest(jev.KindPlanAmendment, jev.AmendmentKey("bound"), "s", "approve", "decline"))
	j.Decide(context.Background(), f.repo, decisionRequest(jev.KindPlanAmendment, jev.AmendmentKey("weak"), "s", "approve", "decline"))
	unconfigured := judge(&settings, p, "")
	unconfigured.Decide(context.Background(), f.repo, decisionRequest(jev.KindPlanAmendment, jev.AmendmentKey("fell"), "s", "approve", "decline"))
	if res := look(jev.AmendmentKey("bound")); res.Status != jev.Binding || res.Pick != "approve" || !res.Recorded {
		t.Fatalf("binding = %+v", res)
	}
	if res := look(jev.AmendmentKey("weak")); res.Status != jev.Nonbinding || res.Pick != "approve" || res.Confidence != 0.4 {
		t.Fatalf("nonbinding = %+v", res)
	}
	if res := look(jev.AmendmentKey("fell")); res.Status != jev.FallbackOnly || res.Pick != "" {
		t.Fatalf("fallback = %+v", res)
	}
	if len(p.Requests()) != 2 {
		t.Fatalf("requests = %d", len(p.Requests()))
	}
	if _, err := j.Lookup(f.repo, streamID, "bogus", "k"); err == nil {
		t.Fatal("an unknown kind was looked up")
	}
}

func TestADecisionUsesTheDefaultThresholdWhenNoneIsConfigured(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	settings.DecisionThreshold = 0
	p := &jevtest.Provider{Results: []jevtest.Result{pick("a", 0.85, "b")}}
	res := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindObjection, jev.ObjectionKey("o"), "s", "a", "b"))
	if res.Status != jev.Nonbinding || res.Threshold != config.DefaultJevDecisionThreshold {
		t.Fatalf("result = %+v", res)
	}
}

// A delivery's occurrence key tells its workstream, final review round and
// pushed commit apart, and its request offers land, pull-request and defer.
func TestDeliveryRouteIsKeyedByWorkstreamReviewAndCommitAndOffersDefer(t *testing.T) {
	keys := map[string]bool{}
	for _, k := range []string{
		jev.DeliveryRouteKey("w_1", 1, "abc"), jev.DeliveryRouteKey("w_1", 2, "abc"), jev.DeliveryRouteKey("w_1", 1, "def"), jev.DeliveryRouteKey("w_2", 1, "abc"),
	} {
		if jev.ValidOccurrence(jev.KindDeliveryRoute, k) != "" {
			t.Fatalf("%q is not a valid occurrence key", k)
		}
		keys[k] = true
	}
	if len(keys) != 4 {
		t.Fatalf("the keys are %v, want 4 different ones", keys)
	}
	if !jev.KindDeliveryRoute.Valid() || jev.KindDeliveryRoute.Task() != "decide-delivery-route" || jev.KindDeliveryRoute.Version() != 1 {
		t.Fatalf("the kind is %s, version %d", jev.KindDeliveryRoute.Task(), jev.KindDeliveryRoute.Version())
	}

	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{pick("land", 0.95, "pull-request", "defer")}}
	key := jev.DeliveryRouteKey("w_1", 1, "abc")
	first := judge(&settings, p, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindDeliveryRoute, key, "state", "land", "pull-request"))
	if first.Status != jev.Binding || first.Pick != "land" {
		t.Fatalf("first = %+v", first)
	}
	var offered []string
	for _, o := range p.Requests()[0].Questions["decision"].Options {
		offered = append(offered, o.Name)
	}
	if strings.Join(offered, ",") != "land,pull-request,defer" {
		t.Fatalf("Jev was offered %v", offered)
	}

	// The recorded result is reused after a restart, and a deferral is
	// recorded as a nonbinding result.
	f.reopen(t)
	again := judge(&settings, &jevtest.Provider{}, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindDeliveryRoute, key, "other state", "land", "pull-request"))
	if again.Status != jev.Binding || again.Pick != "land" || !again.Recorded || again.Judgment != first.Judgment {
		t.Fatalf("again = %+v", again)
	}
	deferred := judge(&settings, &jevtest.Provider{Results: []jevtest.Result{pick("defer", 0.99, "land", "pull-request")}}, "key").Decide(context.Background(), f.repo, decisionRequest(jev.KindDeliveryRoute, jev.DeliveryRouteKey("w_1", 2, "abc"), "state", "land", "pull-request"))
	if deferred.Status != jev.Nonbinding || !deferred.Deferred {
		t.Fatalf("deferred = %+v", deferred)
	}
}
