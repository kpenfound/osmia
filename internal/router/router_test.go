package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func route(t *testing.T, f *fx, x router.Exit) router.Result {
	t.Helper()
	res, err := f.router.Route(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func departing(target string, p float64) *jevfixture.Transitions {
	return &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, target, p, 0.9) }}
}

func TestTheDefaultIsCommittedWhenJevAgrees(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	f.track("implementing")
	x := f.exit()
	res := route(t, f, x)
	if !res.Committed || res.Outcome != "followed" || res.State != "implementing" || res.Journal != router.StateCommitted {
		t.Fatalf("result = %+v", res)
	}
	moves := f.moves()
	if len(moves) != 1 {
		t.Fatalf("%d routed moves", len(moves))
	}
	m := moves[0]
	if m.To != "implementing" || m.From != "checking" || m.DecidedBy != trace.DecidedByCode || m.DefaultTo != "implementing" || m.Judgment == "" || m.Departed() {
		t.Fatalf("transition = %+v", m)
	}
	if strings.Contains(m.Reason, "decide-transition") {
		t.Fatalf("a followed default carries the routing in its reason: %q", m.Reason)
	}
	rec, err := router.Lookup(f.repo, streamID, x)
	if err != nil || rec == nil {
		t.Fatalf("record = %v, %v", rec, err)
	}
	if rec.Table != router.TableVersion || rec.Rule != "3:default_picked" || rec.Judgment != m.Judgment || rec.State != router.StateCommitted {
		t.Fatalf("record = %+v", rec)
	}
	if f.ran("implementing") != 1 {
		t.Fatalf("effect ran %d times", f.ran("implementing"))
	}
	if n := len(f.provider.TransitionRequests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
	spans := f.routeSpans()
	if len(spans) != 1 || attr(spans[0], "osmia.outcome") != "followed" || attr(spans[0], "osmia.trigger") != "unit.checks_failed" || attr(spans[0], "osmia.default_state") != "implementing" || attr(spans[0], "osmia.next_state") != "implementing" || attr(spans[0], "osmia.judgment") != m.Judgment {
		t.Fatalf("route spans = %+v", spans)
	}
}

func TestTheRequestCarriesTheNarrowQuestions(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "findings", Text: "The parser mishandles input.\n```go\nfunc Parse() { panic(1) }\n```\nFix the guard."})
	route(t, f, x)
	reqs := f.provider.TransitionRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	q := reqs[0].Questions
	for _, id := range []string{jev.QuestionFit, jev.QuestionPickA, jev.QuestionPickB, jev.QuestionStuck, jev.QuestionIntent, jev.OwnQuestionPrefix + "environment"} {
		if _, ok := q[id]; !ok {
			t.Fatalf("question %s is missing from %v", id, q)
		}
	}
	a, b := []string{}, []string{}
	for _, o := range q[jev.QuestionPickA].Options {
		a = append(a, o.Name)
	}
	for _, o := range q[jev.QuestionPickB].Options {
		b = append(b, o.Name)
	}
	if len(a) < 2 || len(a) != len(b) || a[0] != b[len(b)-1] || a[len(a)-1] != b[0] {
		t.Fatalf("pick orders %v and %v are not two orders of the same options", a, b)
	}
	for _, banned := range []string{"approved", "abandoned"} {
		if slices.Contains(a, banned) {
			t.Fatalf("%s is offered on an exit without its authority: %v", banned, a)
		}
	}
	state, _ := request(t, f.provider, 0)
	raw, _ := json.Marshal(reqs[0].State)
	body, _ := reqs[0].Body("jev-test")
	if len(body) >= jev.MaxDecisionBytes || strings.Contains(string(raw), "```") || strings.Contains(string(raw), "panic(1)") {
		t.Fatalf("the request holds code or is %d bytes: %s", len(body), raw)
	}
	if state["table"] != "v1" || state["task"] != "decide-transition" || state["default"] != "implementing" || state["minutes_in_state"] == nil {
		t.Fatalf("state = %v", state)
	}
	facts, _ := state["facts"].(map[string]any)
	if facts["bounces"] != float64(1) {
		t.Fatalf("facts = %v", facts)
	}
}

func TestADepartureCommitsAtItsBarAndHoldsBelowIt(t *testing.T) {
	f := newFx(t, departing("checking", 0.8))
	f.seed("checking")
	f.track("checking", "implementing")
	res := route(t, f, f.exit())
	if !res.Committed || res.Outcome != "departed" || res.State != "checking" {
		t.Fatalf("result = %+v", res)
	}
	m := f.moves()[0]
	if m.DecidedBy != trace.DecidedByJev || m.DefaultTo != "implementing" || m.To != "checking" || m.Judgment == "" || !m.Departed() {
		t.Fatalf("transition = %+v", m)
	}
	if f.ran("checking") != 1 || f.ran("implementing") != 0 {
		t.Fatalf("effects ran: checking %d implementing %d", f.ran("checking"), f.ran("implementing"))
	}
	if s := f.routeSpans(); len(s) != 1 || attr(s[0], "osmia.outcome") != "departed" || attr(s[0], "osmia.next_state") != "checking" {
		t.Fatalf("spans = %+v", s)
	}

	g := newFx(t, departing("redrafted", 0.8))
	g.seed("checking")
	res = route(t, g, g.exit())
	if res.Committed || res.Outcome != "held" || res.Hold != router.HoldBelowBar || g.state().Value != "checking" {
		t.Fatalf("result = %+v", res)
	}
	if n := g.notices(); len(n) != 1 || !strings.Contains(n[0], "below its bar") {
		t.Fatalf("notices = %v", n)
	}
	h := newFx(t, departing("redrafted", 0.95))
	h.seed("checking")
	if res = route(t, h, h.exit()); !res.Committed || res.Outcome != "departed" || res.State != "redrafted" {
		t.Fatalf("result = %+v", res)
	}
}

func TestADepartureNeedsItsCorroboration(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.Departs(a, "checking", 0.95, 0.9)
		r.Own["environment"] = 0.2
		return r
	}})
	f.seed("checking")
	res := route(t, f, f.exit())
	if res.Committed || res.Hold != router.HoldBelowBar {
		t.Fatalf("result = %+v", res)
	}
}

func TestDisagreeingOrdersHoldWithOneNotice(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.DefaultFits(a)
		r.PickB = "contested"
		return r
	}})
	f.seed("checking")
	f.track("implementing", "contested")
	res := route(t, f, f.exit())
	if res.Committed || res.Outcome != "held" || res.Hold != router.HoldOrdersDisagree || f.state().Value != "checking" {
		t.Fatalf("result = %+v", res)
	}
	n := f.notices()
	if len(n) != 1 || !strings.Contains(n[0], "Jev's view") || !strings.Contains(n[0], "disagreed") {
		t.Fatalf("notices = %v", n)
	}
	again := route(t, f, f.exit())
	if !again.Reused || again.Journal != router.StateHeld || len(f.notices()) != 1 || len(f.provider.TransitionRequests()) != 1 {
		t.Fatalf("second result = %+v, %d notices, %d requests", again, len(f.notices()), len(f.provider.TransitionRequests()))
	}
	if f.ran("implementing")+f.ran("contested") != 0 || len(f.moves()) != 0 {
		t.Fatal("a held exit ran an effect or moved the unit")
	}
	if s := f.routeSpans(); len(s) != 1 || attr(s[0], "osmia.outcome") != "held" || attr(s[0], "osmia.next_state") != "checking" {
		t.Fatalf("spans = %+v", s)
	}
	held, err := router.Unfinished(f.repo, streamID)
	if err != nil || len(held) != 1 || held[0].State != router.StateHeld {
		t.Fatalf("unfinished = %+v, %v", held, err)
	}
}

func TestNoTargetClearingItsBarHolds(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.Departs(a, "contested", 0.6, 0.9)
		return r
	}})
	f.seed("checking")
	res := route(t, f, f.exit())
	if res.Committed || res.Hold != router.HoldBelowBar || len(f.notices()) != 1 {
		t.Fatalf("result = %+v, notices %v", res, f.notices())
	}
}

func TestWouldChangeIntentGoesToTheOwner(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.Departs(a, "contested", 0.95, 0.9)
		r.Intent = 0.95
		return r
	}})
	f.seed("checking")
	f.track("contested", "implementing")
	res := route(t, f, f.exit())
	if res.Committed || res.Journal != router.StateOwner || res.Target != "contested" || res.Outcome != "held" || f.state().Value != "checking" {
		t.Fatalf("result = %+v", res)
	}
	if n := f.notices(); len(n) != 1 || !strings.Contains(n[0], "owner") {
		t.Fatalf("notices = %v", n)
	}
	if f.ran("contested")+f.ran("implementing") != 0 {
		t.Fatal("an effect ran for a move that belongs to the owner")
	}
	held, _ := router.Unfinished(f.repo, streamID)
	if len(held) != 1 || held[0].State != router.StateOwner || held[0].Target != "contested" {
		t.Fatalf("unfinished = %+v", held)
	}
}

func TestAPickDoesNotBeatADefaultThatFits(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.DefaultFits(a)
		r.PickA, r.PickB = "contested", "contested"
		return r
	}})
	f.seed("checking")
	res := route(t, f, f.exit())
	if !res.Committed || res.State != "implementing" || res.Outcome != "followed" || res.Rule != "4:default_fits" {
		t.Fatalf("result = %+v", res)
	}
}

func TestOneLegalTargetIsFollowedWithoutARequest(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	x.Targets = x.Targets[:1]
	res := route(t, f, x)
	if !res.Committed || res.Outcome != "followed" || res.Rule != "0:only_target" || len(f.provider.Requests()) != 0 {
		t.Fatalf("result = %+v, %d requests", res, len(f.provider.Requests()))
	}
	if m := f.moves()[0]; m.DecidedBy != trace.DecidedByCode || m.Judgment != "" {
		t.Fatalf("transition = %+v", m)
	}
}

func TestNoCandidateFiltersOnlyTargetsThatNeedOne(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("contested")
	x := f.exit()
	x.Trigger, x.Default = "contest.chief_ruling", "revise"
	x.Targets = []router.Target{
		{Name: "revise", Description: "Revise the unit with its mason."},
		{Name: "review", Description: "Review the candidate again.", NeedsCandidate: true},
		{Name: "planned", Description: "Plan the unit again."},
		{Name: "ready", Description: "The unit is ready to start."},
	}
	x.Questions = nil
	x.Prereq.Candidate = func(context.Context) (string, error) { return "the unit has no candidate", nil }
	res := route(t, f, x)
	if !res.Committed || res.State != "revise" {
		t.Fatalf("result = %+v", res)
	}
	_, options := request(t, f.provider, 0)
	if len(options) != 3 || slices.Contains(options, "review") || !slices.Contains(options, "revise") || !slices.Contains(options, "planned") || !slices.Contains(options, "ready") {
		t.Fatalf("options = %v", options)
	}
	g := newFx(t, &jevfixture.Transitions{})
	g.seed("contested")
	y := g.exit()
	y.Prereq.Candidate = nil
	route(t, g, y)
	_, options = request(t, g.provider, 0)
	if slices.Contains(options, "checking") {
		t.Fatalf("options = %v", options)
	}
}

func TestTargetsFailingAPrerequisiteAreNotOffered(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	x.Prereq.Operations = func(_ context.Context, tg router.Target) (string, error) {
		if tg.Name == "contested" {
			return "a landing is in flight", nil
		}
		return "", nil
	}
	x.Prereq.Limits = func(_ context.Context, tg router.Target) (string, error) {
		if tg.Name == "redrafted" {
			return "the budget is exhausted", nil
		}
		return "", nil
	}
	route(t, f, x)
	_, options := request(t, f.provider, 0)
	if slices.Contains(options, "contested") || slices.Contains(options, "redrafted") || len(options) != 2 {
		t.Fatalf("options = %v", options)
	}
}

func TestApprovalIsLegalOnlyWithAReviewerVerdictOrTheOwner(t *testing.T) {
	approval := func(f *fx) {
		f.effects.Register("unit", "approved", func(_ context.Context, p router.Plan) (router.Effect, error) {
			f.applied["approved"].Add(1)
			doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "u1-review", Revision: 1, Project: projectID, Workstream: streamID, Unit: unitID, At: epoch, Actor: checkers, Cause: p.TransitionID},
				Path: "units/u1/review.json", Content: `{"verdict":"satisfactory"}` + "\n"}
			return router.Effect{Documents: []trace.Document{doc}}, nil
		})
	}
	reviewer := func(x router.Exit, valid func(context.Context) (string, error)) router.Exit {
		x.Trigger, x.Default = "unit.review_approved", "approved"
		x.Authority = &router.Authority{Kind: router.AuthorityReviewer, Target: "approved", Valid: valid}
		return x
	}

	f := newFx(t, &jevfixture.Transitions{})
	f.seed("reviewing")
	f.applied["approved"] = new(atomicInt)
	approval(f)
	rx := reviewer(f.exit(), nil)
	res := route(t, f, rx)
	if !res.Committed || res.State != "approved" || res.Outcome != "followed" {
		t.Fatalf("result = %+v", res)
	}
	m := f.moves()[0]
	if m.DecidedBy != trace.DecidedByReviewer || m.To != "approved" || f.ran("approved") != 1 {
		t.Fatalf("transition = %+v, ran %d", m, f.ran("approved"))
	}
	rec, _ := router.Lookup(f.repo, streamID, rx)
	if rec == nil || rec.Authority != router.AuthorityReviewer {
		t.Fatalf("record = %+v", rec)
	}
	_, options := request(t, f.provider, 0)
	if !slices.Contains(options, "approved") {
		t.Fatalf("approved is not offered with a verdict: %v", options)
	}

	g := newFx(t, departing("checking", 0.85))
	g.seed("reviewing")
	g.applied["approved"] = new(atomicInt)
	approval(g)
	res = route(t, g, reviewer(g.exit(), nil))
	if !res.Committed || res.State != "checking" || res.Outcome != "departed" || g.ran("approved") != 0 {
		t.Fatalf("result = %+v", res)
	}

	h := newFx(t, &jevfixture.Transitions{})
	h.seed("reviewing")
	h.applied["approved"] = new(atomicInt)
	approval(h)
	res = route(t, h, reviewer(h.exit(), func(context.Context) (string, error) { return "the candidate changed", nil }))
	if res.Committed || res.Hold != router.HoldDefaultIllegal || h.ran("approved") != 0 || h.state().Value != "reviewing" {
		t.Fatalf("result = %+v", res)
	}

	k := newFx(t, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.Departs(a, "approved", 0.99, 0.99)
		return r
	}})
	k.seed("reviewing")
	k.applied["approved"] = new(atomicInt)
	approval(k)
	res = route(t, k, k.exit())
	if !res.Committed || res.State != "implementing" || res.Outcome != "fallback" || k.ran("approved") != 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, options := request(t, k.provider, 0); slices.Contains(options, "approved") {
		t.Fatalf("approved is offered without authority: %v", options)
	}
}

func TestOwnerGatesOpenOnlyOnAnOwnerOrGateDecision(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	f.track("implementing", "abandoned", "approved")
	proposal := f.exit()
	proposal.Trigger = "unit.move_proposal"
	proposal.Actor = trace.Actor{Kind: "agent", ID: "manager-of-staff"}
	route(t, f, proposal)
	_, options := request(t, f.provider, 0)
	if slices.Contains(options, "abandoned") || slices.Contains(options, "approved") {
		t.Fatalf("a proposal was offered a reserved target: %v", options)
	}

	for _, kind := range []string{router.AuthorityOwner, router.AuthorityJevGate} {
		g := newFx(t, &jevfixture.Transitions{})
		g.seed("checking")
		g.track("abandoned")
		x := g.exit()
		x.Trigger = "unit.move_owner"
		x.Authority = &router.Authority{Kind: kind, Target: "abandoned"}
		if kind == router.AuthorityJevGate {
			x.Authority.Judgment = "judgment_0123"
		}
		res := route(t, g, x)
		if !res.Committed || res.State != "abandoned" || res.Outcome != "followed" || res.Rule != "pinned" || g.ran("abandoned") != 1 {
			t.Fatalf("%s: result = %+v", kind, res)
		}
		if n := len(g.provider.Requests()); n != 0 {
			t.Fatalf("%s: a pinned move made %d requests", kind, n)
		}
		m := g.moves()[0]
		if m.DecidedBy != kind || m.To != "abandoned" || (kind == router.AuthorityJevGate) != (m.Judgment == "judgment_0123") {
			t.Fatalf("%s: transition = %+v", kind, m)
		}
		if s := g.routeSpans(); len(s) != 1 || attr(s[0], "osmia.outcome") != "followed" {
			t.Fatalf("%s: spans = %+v", kind, s)
		}
	}
}

func TestAPrerequisiteChangingWhileWaitingRefusesThePick(t *testing.T) {
	exhausted := false
	provider := departing("checking", 0.9)
	provider.Wait = func(context.Context) error { exhausted = true; return nil }
	f := newFx(t, provider)
	f.seed("checking")
	f.track("checking", "implementing")
	x := f.exit()
	x.Prereq.Limits = func(_ context.Context, tg router.Target) (string, error) {
		if exhausted && tg.Name == "checking" {
			return "the unit's budget is exhausted", nil
		}
		return "", nil
	}
	res := route(t, f, x)
	if res.Committed || res.Journal != router.StateRefused || res.Outcome != "held" || f.state().Value != "checking" {
		t.Fatalf("result = %+v", res)
	}
	if f.ran("checking") != 0 || f.ran("implementing") != 0 || len(f.moves()) != 0 {
		t.Fatal("an effect ran or the unit moved")
	}
	n := f.notices()
	if len(n) != 1 || !strings.Contains(n[0], "budget is exhausted") {
		t.Fatalf("notices = %v", n)
	}
	rec, _ := router.Lookup(f.repo, streamID, f.exit())
	if rec == nil || rec.Refusal == "" {
		t.Fatalf("record = %+v", rec)
	}
	judgments := refused(t, f)
	if len(judgments) != 1 {
		t.Fatalf("%d refused judgments, want 1", len(judgments))
	}
	if held, _ := router.Unfinished(f.repo, streamID); len(held) != 1 {
		t.Fatalf("unfinished = %+v", held)
	}
}

func TestEveryWayOfChoosingATargetIsCheckedWhenApplied(t *testing.T) {
	landing := func(x router.Exit, target string) router.Exit {
		x.Prereq.Operations = func(_ context.Context, tg router.Target) (string, error) {
			if tg.Name == target {
				return "a landing is in flight", nil
			}
			return "", nil
		}
		return x
	}
	t.Run("degraded default", func(t *testing.T) {
		started := false
		provider := &jevfixture.Transitions{Fail: func(int) error { started = true; return &systemone.Error{Kind: systemone.KindTimeout, Message: "slow"} }}
		f := newFx(t, provider)
		f.seed("checking")
		f.track("implementing")
		x := f.exit()
		x.Prereq.Operations = func(_ context.Context, tg router.Target) (string, error) {
			if started && tg.Name == "implementing" {
				return "a landing is in flight", nil
			}
			return "", nil
		}
		res := route(t, f, x)
		if res.Committed || res.Journal != router.StateRefused || f.ran("implementing") != 0 || len(f.notices()) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("pinned move", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.track("abandoned")
		x := landing(f.exit(), "abandoned")
		x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "abandoned"}
		res := route(t, f, x)
		if res.Committed || res.Journal != router.StateRefused || f.ran("abandoned") != 0 || len(f.notices()) != 1 || len(f.provider.Requests()) != 0 {
			t.Fatalf("result = %+v", res)
		}
		if s := f.routeSpans(); len(s) != 1 || attr(s[0], "osmia.outcome") != "held" {
			t.Fatalf("spans = %+v", s)
		}
	})
	t.Run("effect that cannot be carried out", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.effects.Register("unit", "implementing", func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{}, router.Refusal("the unit has no mason thread to resume")
		})
		res := route(t, f, f.exit())
		if res.Committed || res.Journal != router.StateRefused || len(f.notices()) != 1 || len(f.moves()) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestARecordedPickIsFinishedAfterARestart(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "still legal"
		if changed {
			name = "prerequisite changed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFx(t, departing("checking", 0.9))
			f.seed("checking")
			fail := true
			f.effects.Register("unit", "checking", func(_ context.Context, p router.Plan) (router.Effect, error) {
				if fail {
					return router.Effect{}, errors.New("the trace cannot be read now")
				}
				f.applied["checking"].Add(1)
				return router.Effect{}, nil
			})
			f.applied["checking"] = new(atomicInt)
			if _, err := f.router.Route(context.Background(), f.exit()); err == nil {
				t.Fatal("the transient failure was not reported")
			}
			rec, _ := router.Lookup(f.repo, streamID, f.exit())
			if rec == nil || rec.State != router.StateDecided || rec.Target != "checking" || f.state().Value != "checking" || len(f.moves()) != 0 {
				t.Fatalf("record = %+v", rec)
			}
			requests := len(f.provider.Requests())
			f.restart()
			fail = false
			x := f.exit()
			if changed {
				x.Prereq.Limits = func(_ context.Context, tg router.Target) (string, error) {
					if tg.Name == "checking" {
						return "the budget is exhausted", nil
					}
					return "", nil
				}
			}
			res := route(t, f, x)
			if got := len(f.provider.Requests()); got != requests {
				t.Fatalf("recovery sent %d requests", got-requests)
			}
			if changed {
				if res.Committed || res.Journal != router.StateRefused || f.ran("checking") != 0 || len(f.notices()) != 1 || len(refused(t, f)) != 1 {
					t.Fatalf("result = %+v", res)
				}
				return
			}
			if !res.Committed || res.State != "checking" || res.Outcome != "departed" || f.ran("checking") != 1 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestTheSameOccurrenceIsAskedOnce(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	first := route(t, f, x)
	f.restart()
	x.Repo = f.repo
	second := route(t, f, x)
	if !second.Reused || second.Committed != first.Committed || second.Target != first.Target || second.Judgment != first.Judgment {
		t.Fatalf("second = %+v, first = %+v", second, first)
	}
	if len(f.provider.TransitionRequests()) != 1 || len(f.moves()) != 1 {
		t.Fatalf("%d requests, %d moves", len(f.provider.TransitionRequests()), len(f.moves()))
	}
	if s := f.routeSpans(); len(s) != 1 {
		t.Fatalf("%d route spans", len(s))
	}
	other := f.exit()
	other.Cause = "checks-2"
	if key := router.OccurrenceKey(other); key == router.OccurrenceKey(x) {
		t.Fatalf("keys collide: %s", key)
	}
}

func TestAFailedRequestCommitsTheDefaultAsAFallback(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{Fail: func(int) error { return &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"} }})
	f.seed("checking")
	f.track("implementing")
	x := f.exit()
	res := route(t, f, x)
	if !res.Committed || res.Outcome != "fallback" || res.State != "implementing" || f.ran("implementing") != 1 {
		t.Fatalf("result = %+v", res)
	}
	m := f.moves()[0]
	if m.DecidedBy != trace.DecidedByFallback || m.DefaultTo != "implementing" || m.Judgment != "" {
		t.Fatalf("transition = %+v", m)
	}
	rec, _ := router.Lookup(f.repo, streamID, x)
	if rec == nil || rec.Outcome != "fallback" || rec.Fallback == "" || rec.Judgment == "" {
		t.Fatalf("record = %+v", rec)
	}
	if s := f.routeSpans(); len(s) != 1 || attr(s[0], "osmia.outcome") != "fallback" || attr(s[0], "osmia.confidence") != "none" {
		t.Fatalf("spans = %+v", s)
	}
	if status := f.judge.Status(); status.Mode != jev.ModeDegraded {
		t.Fatalf("status = %+v", status)
	}
}

func TestARequestTooLargeFallsBack(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	for i := 0; i < 200; i++ {
		x.Targets = append(x.Targets, router.Target{Name: fmt.Sprintf("target-%03d", i), Description: strings.Repeat("a long description ", 25)})
	}
	res := route(t, f, x)
	if !res.Committed || res.Outcome != "fallback" || res.State != "implementing" {
		t.Fatalf("result = %+v", res)
	}
	if n := len(f.provider.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
	if f.judge.Status().Mode != jev.ModeDegraded {
		t.Fatalf("status = %+v", f.judge.Status())
	}
}

func TestALargeSummaryIsTrimmedToFit(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	for i := 0; i < 100; i++ {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "findings", ID: "f", Text: strings.Repeat("finding text ", 2000)})
	}
	res := route(t, f, x)
	if res.Outcome != "followed" {
		t.Fatalf("result = %+v", res)
	}
	reqs := f.provider.TransitionRequests()
	body, _ := reqs[0].Body("jev-test")
	if len(reqs) != 1 || len(body) > jev.MaxDecisionBytes {
		t.Fatalf("%d requests, %d bytes", len(reqs), len(body))
	}
	state, _ := request(t, f.provider, 0)
	if state["truncated"] != true {
		t.Fatalf("the summary was not marked truncated")
	}
}

func TestEveryPathEntersATargetThroughTheSameEffect(t *testing.T) {
	paths := map[string]func(*fx) router.Exit{
		"default": func(f *fx) router.Exit { return f.exit() },
		"owner": func(f *fx) router.Exit {
			x := f.exit()
			x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "implementing"}
			return x
		},
	}
	for name, build := range paths {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.track("implementing")
		res := route(t, f, build(f))
		if !res.Committed || f.ran("implementing") != 1 {
			t.Fatalf("%s: result = %+v, ran %d", name, res, f.ran("implementing"))
		}
	}
	f := newFx(t, departing("implementing", 0.99))
	f.seed("checking")
	f.track("implementing")
	x := f.exit()
	x.Default = "contested"
	if res := route(t, f, x); !res.Committed || res.Outcome != "departed" || f.ran("implementing") != 1 {
		t.Fatalf("departure: result = %+v, ran %d", res, f.ran("implementing"))
	}
}

func TestAStaleExitCommitsNothing(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	f.seed("reviewing")
	res := route(t, f, x)
	if res.Committed || res.Journal != router.StateStale || len(f.notices()) != 0 || len(f.provider.Requests()) != 0 || f.state().Value != "reviewing" {
		t.Fatalf("result = %+v", res)
	}
}

func TestTheWorkerRoutesBesideTheCaller(t *testing.T) {
	release := make(chan struct{})
	provider := &jevfixture.Transitions{Wait: func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	f := newFx(t, provider)
	f.seed("checking")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := router.NewWorker(ctx, f.router)
	done := make(chan router.Result, 1)
	if !w.Submit(f.exit(), func(r router.Result, err error) {
		if err != nil {
			t.Error(err)
		}
		done <- r
	}) {
		t.Fatal("the exit was not accepted")
	}
	if w.Submit(f.exit(), nil) {
		t.Fatal("an occurrence being routed was accepted twice")
	}
	if len(f.moves()) != 0 {
		t.Fatal("the exit was committed before Jev answered")
	}
	close(release)
	if res := <-done; !res.Committed || res.State != "implementing" {
		t.Fatalf("result = %+v", res)
	}
	w.Wait()
}

func TestRouteOutcomesAndConfidenceBandsMatchTheSpanVocabulary(t *testing.T) {
	outcomes := []string{router.OutcomeFollowed, router.OutcomeDeparted, router.OutcomeHeld, router.OutcomeFallback}
	if fmt.Sprint(outcomes) != fmt.Sprint(recordspans.RouteOutcomes) {
		t.Fatalf("outcomes %v, span %v", outcomes, recordspans.RouteOutcomes)
	}
	bands := []string{router.ConfidenceNone, router.ConfidenceLow, router.ConfidenceModest, router.ConfidenceHigh, router.ConfidenceBinding}
	if fmt.Sprint(bands) != fmt.Sprint(recordspans.ConfidenceBands) {
		t.Fatalf("bands %v, span %v", bands, recordspans.ConfidenceBands)
	}
}

func TestConfidenceBandsAgreeWithTheSpanBands(t *testing.T) {
	for _, p := range []float64{-1, 0, 0.3, 0.49, 0.5, 0.6, 0.7, 0.85, 0.9, 0.95, 1, 2} {
		if got, want := router.ConfidenceBand(p, 0.9), recordspans.ConfidenceBand(p, 0.9); got != want {
			t.Fatalf("band of %v is %s, the span's is %s", p, got, want)
		}
	}
}

func TestEverySpanTriggerIsAValidTrigger(t *testing.T) {
	for _, trigger := range recordspans.RouteTriggers {
		if !router.ValidTrigger(trigger) {
			t.Fatalf("trigger %s is not accepted", trigger)
		}
	}
}

func TestAnExitWithAnInvalidTriggerIsNotRouted(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	x.Trigger = "not a trigger"
	if _, err := f.router.Route(context.Background(), x); err == nil {
		t.Fatal("an exit with an invalid trigger was routed")
	}
}

func TestAnOwnerDecisionApprovesAsAPinnedMove(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	f.applied["approved"] = new(atomicInt)
	f.effects.Register("unit", "approved", func(_ context.Context, p router.Plan) (router.Effect, error) {
		f.applied["approved"].Add(1)
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "u1-review", Revision: 1, Project: projectID, Workstream: streamID, Unit: unitID, At: epoch, Actor: owner, Cause: p.TransitionID},
			Path: "units/u1/review.json", Content: `{"verdict":"satisfactory"}` + "\n"}
		return router.Effect{Documents: []trace.Document{doc}}, nil
	})
	x := f.exit()
	x.Trigger, x.Default, x.Actor = "unit.move_owner", "approved", owner
	x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "approved"}
	res := route(t, f, x)
	if !res.Committed || res.State != "approved" || res.Outcome != "followed" || f.ran("approved") != 1 || len(f.provider.Requests()) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if m := f.moves()[0]; m.DecidedBy != trace.DecidedByOwner || m.Judgment != "" || m.To != "approved" {
		t.Fatalf("transition = %+v", m)
	}

	g := newFx(t, &jevfixture.Transitions{})
	g.seed("checking")
	g.applied["approved"] = new(atomicInt)
	y := g.exit()
	y.Trigger, y.Default = "unit.move_owner", "approved"
	y.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "approved"}
	y.Prereq.Candidate = func(context.Context) (string, error) { return "the unit has no candidate", nil }
	if res := route(t, g, y); res.Committed || res.Journal != router.StateRefused || len(g.notices()) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRecoverFinishesRecordedPicksWithoutTheCaller(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "still legal"
		if changed {
			name = "prerequisite changed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFx(t, departing("checking", 0.9))
			f.seed("checking")
			fail := true
			f.applied["checking"] = new(atomicInt)
			f.effects.Register("unit", "checking", func(context.Context, router.Plan) (router.Effect, error) {
				if fail {
					return router.Effect{}, errors.New("the trace cannot be read now")
				}
				f.applied["checking"].Add(1)
				return router.Effect{}, nil
			})
			if _, err := f.router.Route(context.Background(), f.exit()); err == nil {
				t.Fatal("the transient failure was not reported")
			}
			pending, err := router.Pending(f.repo, streamID)
			if err != nil || len(pending) != 1 || pending[0].Target != "checking" {
				t.Fatalf("pending = %+v, %v", pending, err)
			}
			requests := len(f.provider.Requests())
			f.restart()
			fail = false
			f.router.Rebuild = &router.Rebuilders{}
			f.router.Rebuild.Register("other", func(context.Context, *trace.Repository, router.Record) (router.Exit, error) {
				return router.Exit{}, errors.New("not this machine")
			})
			if n, err := f.router.Recover(context.Background(), f.repo, streamID); err != nil || n != 0 {
				t.Fatalf("recovery without a rebuilder finished %d: %v", n, err)
			}
			f.router.Rebuild.Register("unit", func(_ context.Context, _ *trace.Repository, rec router.Record) (router.Exit, error) {
				x := f.exit()
				if rec.Actor != checkers {
					t.Errorf("the record lost its actor: %+v", rec.Actor)
				}
				if changed {
					x.Prereq.Limits = func(_ context.Context, tg router.Target) (string, error) {
						if tg.Name == "checking" {
							return "the budget is exhausted", nil
						}
						return "", nil
					}
				}
				return x, nil
			})
			n, err := f.router.Recover(context.Background(), f.repo, streamID)
			if err != nil || n != 1 {
				t.Fatalf("recovery finished %d: %v", n, err)
			}
			if got := len(f.provider.Requests()); got != requests {
				t.Fatalf("recovery sent %d requests", got-requests)
			}
			if pending, _ := router.Pending(f.repo, streamID); len(pending) != 0 {
				t.Fatalf("still pending: %+v", pending)
			}
			if changed {
				if f.ran("checking") != 0 || len(f.notices()) != 1 || len(refused(t, f)) != 1 || len(f.moves()) != 0 {
					t.Fatalf("a changed prerequisite was applied: ran %d, notices %d", f.ran("checking"), len(f.notices()))
				}
				return
			}
			if f.ran("checking") != 1 || f.state().Value != "checking" || len(f.moves()) != 1 || f.moves()[0].DecidedBy != trace.DecidedByJev {
				t.Fatalf("the recorded pick was not applied: ran %d, moves %+v", f.ran("checking"), f.moves())
			}
		})
	}
}

func worksExit(f *fx) router.Exit {
	x := f.exit()
	for i := range x.Targets {
		switch x.Targets[i].Name {
		case "implementing", "checking":
			x.Targets[i].Works = true
		}
	}
	return x
}

func TestStoppedWorkStopsTheMovesThatStartIt(t *testing.T) {
	const why = "work is paused (daily-budget): the budget is spent"
	t.Run("the targets are not offered", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.router.Stops = func(context.Context, router.Exit) (string, error) { return why, nil }
		x := worksExit(f)
		x.Default = "contested"
		x.Trigger = "unit.move_proposal"
		route(t, f, x)
		_, options := request(t, f.provider, 0)
		if slices.Contains(options, "implementing") || slices.Contains(options, "checking") || !slices.Contains(options, "contested") {
			t.Fatalf("options = %v", options)
		}
	})

	t.Run("a pick is refused when work stops while Jev answers", func(t *testing.T) {
		stopped := false
		provider := departing("checking", 0.9)
		provider.Wait = func(context.Context) error { stopped = true; return nil }
		f := newFx(t, provider)
		f.seed("checking")
		f.track("checking", "implementing", "contested")
		f.router.Stops = func(context.Context, router.Exit) (string, error) {
			if stopped {
				return why, nil
			}
			return "", nil
		}
		x := worksExit(f)
		x.Default = "contested"
		res := route(t, f, x)
		if res.Committed || res.Journal != router.StateRefused || res.Outcome != router.OutcomeHeld || f.state().Value != "checking" {
			t.Fatalf("result = %+v", res)
		}
		if f.ran("checking") != 0 || f.ran("contested") != 0 || len(f.moves()) != 0 {
			t.Fatal("an effect ran or the unit moved")
		}
		if n := f.notices(); len(n) != 1 || !strings.Contains(n[0], "budget is spent") {
			t.Fatalf("notices = %v", n)
		}
	})

	t.Run("a default that starts work is held", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.router.Stops = func(context.Context, router.Exit) (string, error) { return why, nil }
		res := route(t, f, worksExit(f))
		if res.Committed || res.Hold != router.HoldDefaultIllegal || len(f.moves()) != 0 || len(f.notices()) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("a default entered through its trigger's own records is held as well", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.router.Stops = func(context.Context, router.Exit) (string, error) { return why, nil }
		x := worksExit(f)
		entered := false
		x.Enter = func(context.Context, router.Plan) (router.Effect, error) { entered = true; return router.Effect{}, nil }
		res := route(t, f, x)
		if res.Committed || res.Hold != router.HoldDefaultIllegal || entered || len(f.moves()) != 0 || f.state().Value != "checking" || len(f.notices()) != 1 {
			t.Fatalf("result = %+v, entered %v", res, entered)
		}
	})

	t.Run("a default entered through its trigger's own records goes on while nothing stops it", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.router.Stops = func(context.Context, router.Exit) (string, error) { return "", nil }
		x := worksExit(f)
		x.Enter = func(context.Context, router.Plan) (router.Effect, error) { return router.Effect{}, nil }
		res := route(t, f, x)
		if !res.Committed || res.State != "implementing" || res.Outcome != router.OutcomeFollowed {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("an owner's pinned decision meets the same limit", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.track("implementing")
		f.router.Stops = func(context.Context, router.Exit) (string, error) { return why, nil }
		x := worksExit(f)
		x.Trigger, x.Actor = "unit.move_owner", owner
		x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "implementing"}
		res := route(t, f, x)
		if res.Committed || res.Journal != router.StateRefused || f.ran("implementing") != 0 || len(f.moves()) != 0 || f.state().Value != "checking" || len(f.provider.Requests()) != 0 {
			t.Fatalf("result = %+v", res)
		}
		if n := f.notices(); len(n) != 1 || !strings.Contains(n[0], "budget is spent") {
			t.Fatalf("notices = %v", n)
		}
	})

	t.Run("an owner's pinned decision goes on while nothing stops it", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.router.Stops = func(context.Context, router.Exit) (string, error) { return "", nil }
		x := worksExit(f)
		x.Trigger, x.Actor = "unit.move_owner", owner
		x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "implementing"}
		res := route(t, f, x)
		if !res.Committed || res.State != "implementing" || len(f.provider.Requests()) != 0 {
			t.Fatalf("result = %+v", res)
		}
		if moves := f.moves(); len(moves) != 1 || moves[0].DecidedBy != trace.DecidedByOwner {
			t.Fatalf("moves = %+v", moves)
		}
	})
}

func TestAPauseHoldsAnExitUntilItLifts(t *testing.T) {
	const why = "work is paused (owner): resting"
	t.Run("a default is held and then entered", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		paused := true
		f.router.Paused = func(context.Context, router.Exit) (string, error) {
			if paused {
				return why, nil
			}
			return "", nil
		}
		entered := 0
		x := worksExit(f)
		x.Enter = func(context.Context, router.Plan) (router.Effect, error) { entered++; return router.Effect{}, nil }
		res := route(t, f, x)
		if res.Committed || res.Hold != router.HoldPaused || res.Outcome != router.OutcomeHeld || res.Journal != "" || entered != 0 {
			t.Fatalf("result = %+v", res)
		}
		if f.state().Value != "checking" || len(f.moves()) != 0 || len(f.notices()) != 0 || len(f.provider.Requests()) != 0 {
			t.Fatal("a held exit moved, was noticed or asked Jev")
		}
		if rec, err := router.Lookup(f.repo, streamID, x); err != nil || rec != nil {
			t.Fatalf("a held exit left a record: %+v, %v", rec, err)
		}
		paused = false
		res = route(t, f, x)
		if !res.Committed || res.State != "implementing" || entered != 1 {
			t.Fatalf("result after the pause = %+v", res)
		}
	})

	t.Run("a pick recorded before the pause is applied once it lifts", func(t *testing.T) {
		paused := false
		provider := departing("checking", 0.9)
		provider.Wait = func(context.Context) error { paused = true; return nil }
		f := newFx(t, provider)
		f.seed("checking")
		f.track("checking", "implementing")
		f.router.Paused = func(context.Context, router.Exit) (string, error) {
			if paused {
				return why, nil
			}
			return "", nil
		}
		x := worksExit(f)
		res := route(t, f, x)
		if res.Committed || res.Hold != router.HoldPaused || f.ran("checking") != 0 || len(f.notices()) != 0 {
			t.Fatalf("result = %+v", res)
		}
		requests := len(f.provider.Requests())
		paused = false
		res = route(t, f, x)
		if !res.Committed || res.State != "checking" || f.ran("checking") != 1 || len(f.provider.Requests()) != requests {
			t.Fatalf("result after the pause = %+v", res)
		}
	})

	t.Run("an owner's pinned decision waits for the pause", func(t *testing.T) {
		f := newFx(t, &jevfixture.Transitions{})
		f.seed("checking")
		f.track("implementing")
		f.router.Paused = func(context.Context, router.Exit) (string, error) { return why, nil }
		x := worksExit(f)
		x.Trigger, x.Actor = "unit.move_owner", owner
		x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "implementing"}
		res := route(t, f, x)
		if res.Committed || res.Hold != router.HoldPaused || f.ran("implementing") != 0 || len(f.moves()) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestAManagerMoveIsPinnedButOpensNoOwnerGate(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	f.track("implementing")
	x := f.exit()
	x.Trigger, x.Default = "unit.move_proposal", "implementing"
	x.Actor = trace.Actor{Kind: "agent", ID: trace.Manager}
	x.Authority = &router.Authority{Kind: router.AuthorityManager, Target: "implementing"}
	res := route(t, f, x)
	if !res.Committed || res.State != "implementing" || res.Outcome != "followed" || res.Rule != "pinned" || f.ran("implementing") != 1 {
		t.Fatalf("result = %+v", res)
	}
	if n := len(f.provider.Requests()); n != 0 {
		t.Fatalf("a manager move made %d requests", n)
	}
	if m := f.moves()[0]; m.DecidedBy != trace.DecidedByManager || m.Judgment != "" || !trace.ValidRouting(m.To, m.Routing()) {
		t.Fatalf("transition = %+v", m)
	}

	for _, target := range []string{"abandoned", "approved"} {
		g := newFx(t, &jevfixture.Transitions{})
		g.seed("checking")
		g.track(target)
		y := g.exit()
		y.Trigger, y.Default = "unit.move_proposal", target
		y.Actor = trace.Actor{Kind: "agent", ID: trace.Manager}
		y.Authority = &router.Authority{Kind: router.AuthorityManager, Target: target}
		if res := route(t, g, y); res.Committed || g.ran(target) != 0 || g.state().Value != "checking" {
			t.Fatalf("%s: result = %+v", target, res)
		}
	}
}
