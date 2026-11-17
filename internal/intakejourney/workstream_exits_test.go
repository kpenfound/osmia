package intakejourney

import (
	"fmt"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// With the fixture Jev that finds the default fits, a workstream whose drafts
// were all refused stops drafting as it always has, and the transition records
// the default it took.
func TestADraftExitThatFitsItsDefaultStopsDrafting(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	p := &jevfixture.Transitions{}
	service.InspectForTest(f.s.core()).SetJudge(jevfixture.TransitionJudge(f.clock.Now, p))
	for n := 1; n <= maxDrafts+1; n++ {
		f.script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan}, nil)
	}
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, draftAt("exhausted"))

	rec, ok := f.routeOf(t, stream, intake.TriggerDraftExhausted)
	if !ok || rec.Outcome != router.OutcomeFollowed || rec.Default != intake.TargetExhausted || rec.Target != intake.TargetExhausted || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	var stopped trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.ID == "draft-exhausted" {
			stopped = tr
		}
	}
	if stopped.To != "exhausted" || stopped.DefaultTo != "exhausted" || stopped.DecidedBy != trace.DecidedByCode || stopped.Departed() {
		t.Fatalf("the transition %+v", stopped)
	}
	if runs := f.runs(); len(runs) != maxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
}

// A scripted departure after the drafts are exhausted asks the architect for one
// more draft when it clears the strict bar: the draft subject enters drafting-4
// with Jev as its decider, the departure is recorded and shown, and the new
// draft runs and is accepted.
func TestAScriptedDepartureAfterTheDraftsAreExhaustedAsksForOneMoreDraft(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
		if ask.Trigger == intake.TriggerDraftExhausted {
			// One more draft needs the failures to be fixable and not to repeat.
			reply := jevfixture.Departs(ask, intake.TargetNewDraft, 0.96, 0.96)
			reply.Own["failures_repeat"] = 0.05
			return reply
		}
		return jevfixture.DefaultFits(ask)
	}}
	service.InspectForTest(f.s.core()).SetJudge(jevfixture.TransitionJudge(f.clock.Now, p))
	for n := 1; n <= maxDrafts; n++ {
		f.script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan}, nil)
	}
	f.script(fmt.Sprintf("draft-%d-1", maxDrafts+1), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)

	if runs := f.runs(); len(runs) != maxDrafts+1 {
		t.Fatalf("backend runs %v", runs)
	}
	var more trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == intake.DraftSubject && tr.To == fmt.Sprintf("drafting-%d", maxDrafts+1) {
			more = tr
		}
	}
	if more.DecidedBy != trace.DecidedByJev || more.DefaultTo != "exhausted" || more.Judgment == "" || !more.Departed() {
		t.Fatalf("the departure %+v", more)
	}
	rec, ok := f.routeOf(t, stream, intake.TriggerDraftExhausted)
	if !ok || rec.Outcome != router.OutcomeDeparted || rec.Target != intake.TargetNewDraft || rec.Default != intake.TargetExhausted || rec.Judgment != more.Judgment || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	feed, api := service.InspectForTest(f.s.core()).Feed(string(stream))
	if api != nil {
		t.Fatal(api.Message)
	}
	departed := 0
	for _, e := range feed.Entries {
		if e.Transition != nil && e.Transition.Departed {
			departed++
		}
	}
	if departed != 1 {
		t.Fatalf("the feed shows %d departures, want 1", departed)
	}
}
