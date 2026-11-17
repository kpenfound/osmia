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

// MaxDrafts stays a hard limit: once the drafts are exhausted another draft is
// not offered, so the exit has the one target, the router asks Jev nothing even
// when Jev is scripted to ask for one more draft, and no further draft runs.
func TestADraftLimitOffersNoRetry(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(ask, "new_draft", 0.99, 0.99)
	}}
	service.InspectForTest(f.s.core()).SetJudge(jevfixture.TransitionJudge(f.clock.Now, p))
	for n := 1; n <= maxDrafts+1; n++ {
		f.script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan}, nil)
	}
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, draftAt("exhausted"))

	rec, ok := f.routeOf(t, stream, intake.TriggerDraftExhausted)
	if !ok || rec.Outcome != router.OutcomeFollowed || rec.Target != intake.TargetExhausted || rec.JevRequested || len(rec.Offered) > 1 {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a hard limit with one legal target", n)
	}
	if runs := f.runs(); len(runs) != maxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
}
