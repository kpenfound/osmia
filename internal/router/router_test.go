package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// departing scripts Jev to depart from the default to target at p, with every
// own question corroborating.
func departing(target string, p float64) *jevfixture.Transitions {
	return &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, target, p, 0.9) }}
}

// When Jev agrees, the router commits the default through the Transaction with
// the routing filled in, sends one request with the narrow questions, and
// emits one followed route span.
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
	if !strings.Contains(m.Reason, "table v1") || !strings.Contains(m.Reason, "3:default_picked") {
		t.Fatalf("the table version and rule are not in the reason: %q", m.Reason)
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

// The request carries the narrow questions with the target question in two
// orders, offers only legal targets, holds no code, stays under 72 KiB, and its
// numbers come from code.
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
	// approved and abandoned need an authority this exit does not carry.
	for _, banned := range []string{"approved", "abandoned"} {
		if contains(a, banned) {
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

// A departed target that clears its bar is committed, recorded as Jev's
// departure from the default; one below its bar is held.
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

	// The strict bar is the decision threshold: 0.8 is under 0.9.
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

// A trigger answer that does not corroborate a departure keeps it from
// committing, whatever the picks say.
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

// When the two orderings disagree the subject is held without moving, with
// exactly one chief notice for the occurrence, however often the exit is raised.
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

// A default judged not to fit with no target clearing its bar is a hold too.
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

// "Would change intent" sends the move to the owner instead of committing it.
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

// A default that fits beats a different pick, even a confident one.
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

// With one legal target left there is nothing to choose between: the router
// keeps the default itself and Jev is not asked.
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

// Candidate-exists filters only the targets that need a candidate: an exit with
// no candidate still offers planned, ready and a revise target.
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
	if len(options) != 3 || contains(options, "review") || !contains(options, "revise") || !contains(options, "planned") || !contains(options, "ready") {
		t.Fatalf("options = %v", options)
	}
	// An exit that supplies no candidate check offers none of those targets.
	g := newFx(t, &jevfixture.Transitions{})
	g.seed("contested")
	y := g.exit()
	y.Prereq.Candidate = nil
	route(t, g, y)
	_, options = request(t, g.provider, 0)
	if contains(options, "checking") {
		t.Fatalf("options = %v", options)
	}
}

// A target that fails a prerequisite never reaches Jev.
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
	if contains(options, "contested") || contains(options, "redrafted") || len(options) != 2 {
		t.Fatalf("options = %v", options)
	}
}

// Approval is legal only on an exit that carries a satisfactory reviewer verdict
// for the current candidate or an owner decision, and the router applies it
// through the shared approval effect and records the authority.
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

	// A reviewer verdict: approved is offered, applied with the approval record,
	// and the authority is the verdict's.
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
	if !contains(options, "approved") {
		t.Fatalf("approved is not offered with a verdict: %v", options)
	}

	// Jev may depart from an authorised approval, to another target at its bar,
	// and the verdict is untouched.
	g := newFx(t, departing("checking", 0.85))
	g.seed("reviewing")
	g.applied["approved"] = new(atomicInt)
	approval(g)
	res = route(t, g, reviewer(g.exit(), nil))
	if !res.Committed || res.State != "checking" || res.Outcome != "departed" || g.ran("approved") != 0 {
		t.Fatalf("result = %+v", res)
	}

	// A verdict that is no longer the current one makes approved illegal: the
	// default is not offered and the subject is held.
	h := newFx(t, &jevfixture.Transitions{})
	h.seed("reviewing")
	h.applied["approved"] = new(atomicInt)
	approval(h)
	res = route(t, h, reviewer(h.exit(), func(context.Context) (string, error) { return "the candidate changed", nil }))
	if res.Committed || res.Hold != router.HoldDefaultIllegal || h.ran("approved") != 0 || h.state().Value != "reviewing" {
		t.Fatalf("result = %+v", res)
	}

	// Without the authority approved is never offered, and a scripted answer
	// that names it is not applied: the default commits in degraded mode.
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
	if _, options := request(t, k.provider, 0); contains(options, "approved") {
		t.Fatalf("approved is offered without authority: %v", options)
	}
}

// A chief proposal carries no authority: neither approved nor an owner gate is
// ever offered or applied on it, and an owner decision commits exactly its
// target through the shared effects without a request.
func TestOwnerGatesOpenOnlyOnAnOwnerOrGateDecision(t *testing.T) {
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	f.track("implementing", "abandoned", "approved")
	proposal := f.exit()
	proposal.Trigger = "unit.move_proposal"
	proposal.Actor = trace.Actor{Kind: "agent", ID: "chief-of-staff"}
	route(t, f, proposal)
	_, options := request(t, f.provider, 0)
	if contains(options, "abandoned") || contains(options, "approved") {
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

// A hard prerequisite that changes while the router waits for Jev (the budget
// runs out, a landing starts) leaves the version alone: the pick is not
// applied, Refuse is recorded, no effect runs, and the subject is held with one
// notice.
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

// The same holds for a landing that starts without any state version change,
// for a degraded-mode default and for a pinned move.
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

// A pick that is recorded but not applied when the service restarts is applied
// on recovery, without a new request, if it is still legal; and refused if a
// prerequisite changed in between.
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

// A second exit with the same occurrence key, after a restart too, reuses the
// recorded result without a request, a commit or a second span.
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
	// Another cause is another occurrence, though the state version and the
	// trigger are the same.
	other := f.exit()
	other.Cause = "checks-2"
	if key := router.OccurrenceKey(other); key == router.OccurrenceKey(x) {
		t.Fatalf("keys collide: %s", key)
	}
}

// A request that fails commits the still-legal default as a fallback, the
// record and the span say so, and Jev is degraded in the status.
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

// A request too large to send falls back without being sent, and shows Jev as
// degraded.
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

// A large history and long evidence are trimmed to fit under 72 KiB, so the
// request is sent.
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

// The shared effects of a target are the same whoever chooses it.
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

// The exit's subject having moved on since it was raised is a stale exit: no
// commit, no notice.
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

// The worker routes an exit beside the caller: Submit returns before Jev answers,
// and an occurrence being routed is not submitted twice.
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

// The router's words for the route span are the span's own: the outcomes, the
// confidence bands and the triggers it can carry.
func TestTheRouteVocabularyIsTheSpans(t *testing.T) {
	outcomes := []string{router.OutcomeFollowed, router.OutcomeDeparted, router.OutcomeHeld, router.OutcomeFallback}
	if fmt.Sprint(outcomes) != fmt.Sprint(recordspans.RouteOutcomes) {
		t.Fatalf("outcomes %v, span %v", outcomes, recordspans.RouteOutcomes)
	}
	bands := []string{router.ConfidenceNone, router.ConfidenceLow, router.ConfidenceModest, router.ConfidenceHigh, router.ConfidenceBinding}
	if fmt.Sprint(bands) != fmt.Sprint(recordspans.ConfidenceBands) {
		t.Fatalf("bands %v, span %v", bands, recordspans.ConfidenceBands)
	}
	for _, p := range []float64{-1, 0, 0.3, 0.49, 0.5, 0.6, 0.7, 0.85, 0.9, 0.95, 1, 2} {
		if got, want := router.ConfidenceBand(p, 0.9), recordspans.ConfidenceBand(p, 0.9); got != want {
			t.Fatalf("band of %v is %s, the span's is %s", p, got, want)
		}
	}
	for _, trigger := range recordspans.RouteTriggers {
		if !router.ValidTrigger(trigger) {
			t.Fatalf("trigger %s is not accepted", trigger)
		}
	}
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	x := f.exit()
	x.Trigger = "not a trigger"
	if _, err := f.router.Route(context.Background(), x); err == nil {
		t.Fatal("an exit with no trigger was routed")
	}
}

// An owner's decision makes approved legal as a pinned move: it commits with the
// approval record and no request, and without a candidate it is refused.
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

// Recover finishes, with no caller raising the exit again, a pick that was
// recorded but not applied when the service stopped: a pick that is still
// legal is applied without a new request, one whose prerequisite changed is
// refused with one notice, and a machine that cannot rebuild its exit is left
// alone.
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
