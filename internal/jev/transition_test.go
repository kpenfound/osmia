package jev_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/jev"
)

func transitionRequest(occurrence string) jev.TransitionRequest {
	return jev.TransitionRequest{
		Scope: decisionScope(), Cause: "request_1", Occurrence: occurrence, State: map[string]any{"trigger": "unit.checks_failed", "default": "implementing"},
		Options:   []jev.TransitionOption{{Name: "implementing", Description: "Send it back."}, {Name: "checking", Description: "Run again."}, {Name: "contested", Description: "Hold."}},
		Questions: []jev.TransitionQuestion{{Name: "environment", Instructions: "The failure is infrastructure."}},
	}
}

func transitionAnswer(pickA, pickB string, fit float64) jevtest.Result {
	opts := []string{"implementing", "checking", "contested"}
	others := func(c string) []string {
		var out []string
		for _, o := range opts {
			if o != c {
				out = append(out, o)
			}
		}
		return out
	}
	return jevtest.Result{Response: systemone.Response{Model: "jev-1.13.0", Usage: systemone.Usage{InputTokens: 100, OutputTokens: 5, CostUSD: 0.000004, CostKnown: true}, Answers: map[string]systemone.Answer{
		jev.QuestionFit: jevtest.Noul(fit), jev.QuestionStuck: jevtest.Noul(0.2), jev.QuestionIntent: jevtest.Noul(0.1), jev.OwnQuestionPrefix + "environment": jevtest.Noul(0.8),
		jev.QuestionPickA: jevtest.Choice(pickA, 0.8, 0.8, others(pickA)...), jev.QuestionPickB: jevtest.Choice(pickB, 0.7, 0.7, others(pickB)...),
	}}}
}

// A transition request asks the narrow questions in one request, with the pick
// asked in two orders, and the answers are recorded under the versioned task.
func TestDecideTransitionAsksTheNarrowQuestionsInOneRequest(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{transitionAnswer("checking", "checking", 0.2)}}
	res := judge(&settings, p, "key").DecideTransition(context.Background(), f.repo, transitionRequest("unit-u1@2/unit.checks_failed/checks-1"))
	if !res.Answered || res.Recorded || res.Judgment == "" {
		t.Fatalf("result = %+v", res)
	}
	a := res.Answers
	if a.Fit != 0.2 || a.Stuck != 0.2 || a.Intent != 0.1 || a.PickA != (jev.TransitionPick{Choice: "checking", Confidence: 0.8}) || a.PickB != (jev.TransitionPick{Choice: "checking", Confidence: 0.7}) || a.Own["environment"] != 0.8 {
		t.Fatalf("answers = %+v", a)
	}
	reqs := p.Requests()
	if len(reqs) != 1 || len(reqs[0].Questions) != 6 {
		t.Fatalf("%d requests: %+v", len(reqs), reqs)
	}
	first, second := reqs[0].Questions[jev.QuestionPickA].Options, reqs[0].Questions[jev.QuestionPickB].Options
	if len(first) != 3 || first[0].Name != second[2].Name || first[2].Name != second[0].Name || first[1].Name != second[1].Name {
		t.Fatalf("orders %v and %v", first, second)
	}
	for _, r := range f.judgmentRecords(t) {
		if r.State == "accepted" && (r.Task != "decide-transition" || r.Version != jev.TransitionVersion || r.Occurrence == nil || r.Occurrence.Kind != jev.TransitionKind) {
			t.Fatalf("record = %+v", r)
		}
	}
}

// An answered occurrence is not asked again, after a restart either, and a
// concurrent ask of it waits for the first.
func TestDecideTransitionReusesARecordedAnswer(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{transitionAnswer("checking", "checking", 0.2)}}
	j := judge(&settings, p, "key")
	req := transitionRequest("unit-u1@2/unit.checks_failed/checks-1")
	first := j.DecideTransition(context.Background(), f.repo, req)
	f.reopen(t)
	later := &jevtest.Provider{Results: []jevtest.Result{transitionAnswer("contested", "contested", 0.9)}}
	settings.Model = "jev-1.14.0"
	req.Options = req.Options[:2]
	again := judge(&settings, later, "key").DecideTransition(context.Background(), f.repo, req)
	if !again.Recorded || again.Judgment != first.Judgment || again.Answers.PickA.Choice != "checking" || len(later.Requests()) != 0 {
		t.Fatalf("again = %+v, %d requests", again, len(later.Requests()))
	}
}

// A request that fails is a recorded fallback with no answers and marks Jev
// degraded until a request is answered; the other kinds are not touched.
func TestDecideTransitionFailureDegradesJevUntilAnAnswer(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindRejected, Message: "no"}}, transitionAnswer("implementing", "implementing", 0.9)}}
	j := judge(&settings, p, "key")
	if j.Status().Mode != jev.ModeReady {
		t.Fatalf("status = %+v", j.Status())
	}
	res := j.DecideTransition(context.Background(), f.repo, transitionRequest("unit-u1@2/unit.checks_failed/checks-1"))
	if res.Answered || res.Reason != jev.ReasonRejected || res.Judgment == "" {
		t.Fatalf("result = %+v", res)
	}
	if s := j.Status(); s.Mode != jev.ModeDegraded || s.Reason != jev.ReasonRejected {
		t.Fatalf("status = %+v", s)
	}
	res = j.DecideTransition(context.Background(), f.repo, transitionRequest("unit-u1@3/unit.checks_failed/checks-2"))
	if !res.Answered || j.Status().Mode != jev.ModeReady {
		t.Fatalf("result = %+v, status = %+v", res, j.Status())
	}
}

// An answer that picks something that was not offered is not used, and a
// request over the limit is not sent.
func TestDecideTransitionRefusesWhatWasNotOfferedAndWhatIsTooLarge(t *testing.T) {
	f := newFixture(t)
	settings := testSettings()
	p := &jevtest.Provider{Results: []jevtest.Result{transitionAnswer("approved", "approved", 0.9)}}
	j := judge(&settings, p, "key")
	res := j.DecideTransition(context.Background(), f.repo, transitionRequest("unit-u1@2/unit.checks_failed/checks-1"))
	if res.Answered || res.Reason != jev.ReasonDeclined {
		t.Fatalf("result = %+v", res)
	}
	large := transitionRequest("unit-u1@2/unit.checks_failed/checks-3")
	large.State = map[string]string{"text": strings.Repeat("x", jev.MaxDecisionBytes)}
	n := len(p.Requests())
	res = j.DecideTransition(context.Background(), f.repo, large)
	if res.Answered || res.Reason != jev.ReasonRejected || len(p.Requests()) != n {
		t.Fatalf("result = %+v", res)
	}
	one := transitionRequest("unit-u1@2/unit.checks_failed/checks-4")
	one.Options = one.Options[:1]
	if res = j.DecideTransition(context.Background(), f.repo, one); res.Answered || res.Reason != jev.ReasonRejected {
		t.Fatalf("one option: %+v", res)
	}
}
