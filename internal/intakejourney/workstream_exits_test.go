package intakejourney

import (
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestADraftExitThatFitsItsDefaultStopsDrafting(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	p := &jevfixture.Transitions{}
	service.InspectForTest(f.S.Core()).SetJudge(jevfixture.TransitionJudge(f.Clock.Now, p))
	for n := 1; n <= intake.MaxDrafts+1; n++ {
		f.Script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan}, nil)
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.DraftAt("exhausted"))

	rec, ok := f.RouteOf(t, stream, intake.TriggerDraftExhausted)
	if !ok || rec.Outcome != router.OutcomeFollowed || rec.Default != intake.TargetExhausted || rec.Target != intake.TargetExhausted || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	var stopped trace.Transition
	for _, tr := range f.Transitions(t, stream) {
		if tr.ID == "draft-exhausted" {
			stopped = tr
		}
	}
	if stopped.To != "exhausted" || stopped.DefaultTo != "exhausted" || stopped.DecidedBy != trace.DecidedByCode || stopped.Departed() {
		t.Fatalf("the transition %+v", stopped)
	}
	if runs := f.Runs(); len(runs) != intake.MaxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
}

func TestADraftLimitOffersNoRetry(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(ask, "new_draft", 0.99, 0.99)
	}}
	service.InspectForTest(f.S.Core()).SetJudge(jevfixture.TransitionJudge(f.Clock.Now, p))
	for n := 1; n <= intake.MaxDrafts+1; n++ {
		f.Script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan}, nil)
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.DraftAt("exhausted"))

	rec, ok := f.RouteOf(t, stream, intake.TriggerDraftExhausted)
	if !ok || rec.Outcome != router.OutcomeFollowed || rec.Target != intake.TargetExhausted || rec.JevRequested || len(rec.Offered) > 1 {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a hard limit with one legal target", n)
	}
	if runs := f.Runs(); len(runs) != intake.MaxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
}
