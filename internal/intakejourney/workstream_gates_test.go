package intakejourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"slices"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

type triggersAsked struct {
	mu       sync.Mutex
	triggers []string
	reply    func(jevfixture.Ask) jevfixture.Reply
}

func (a *triggersAsked) script(ask jevfixture.Ask) jevfixture.Reply {
	a.mu.Lock()
	a.triggers = append(a.triggers, ask.Trigger)
	a.mu.Unlock()
	if a.reply != nil {
		return a.reply(ask)
	}
	return jevfixture.DefaultFits(ask)
}

func (a *triggersAsked) asked(trigger string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.triggers, trigger)
}

func TestTheOwnersRatificationIsAPinnedMove(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{}
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := newJevJourney(t, p, true)
	defer f.Stop(t)
	ctx := context.Background()

	stream := f.ReachJevPacket(t)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	if _, err := f.C.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.Scenario().AwaitFeature(t, stream, intake.BuildingState)

	ratified := f.AwaitSubjectMove(t, stream, intake.OwnerSubject, "ratified-1")
	if ratified.DecidedBy != trace.DecidedByOwner || ratified.Judgment != "" || ratified.DefaultTo != "ratified-1" || ratified.Departed() || ratified.Actor != intake.OwnerActor {
		t.Fatalf("the owner's ratification %+v", ratified)
	}
	if record := f.Ratification(t, stream, 1); record.DecidedBy == trace.DecidedByJev || record.Judgment != "" {
		t.Fatalf("the ratification file %+v", record)
	}
	rec, ok := f.RouteOf(t, stream, intake.TriggerRatified)
	if !ok || rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.State != router.StateCommitted || rec.Target != "ratified" {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if asked.asked(intake.TriggerRatified) {
		t.Fatal("Jev was asked about an owner's ratification")
	}
}

func TestAJevGateRatificationIsAPinnedMoveWithItsJudgment(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{}
	p := &jevfixture.Provider{Confidence: 0.97, Transition: asked.script}
	f := newJevJourney(t, p, true)
	defer f.Stop(t)

	stream := f.ReachJevPacket(t)
	f.Scenario().AwaitFeature(t, stream, intake.BuildingState)

	ratified := f.AwaitSubjectMove(t, stream, intake.OwnerSubject, "ratified-1")
	docs := f.Documents(t, stream, shed.RatificationDocumentID(1))
	if len(docs) != 1 {
		t.Fatalf("ratification records: %+v", docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	if ratified.DecidedBy != trace.DecidedByJevGate || ratified.Judgment == "" || ratified.Judgment != record.Judgment || ratified.DefaultTo != "ratified-1" || ratified.Departed() || ratified.Actor != trace.JevActor {
		t.Fatalf("Jev's ratification %+v, file %+v", ratified, record)
	}
	rec, ok := f.RouteOf(t, stream, intake.TriggerRatified)
	if !ok || rec.Authority != router.AuthorityJevGate || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.DecidedBy != trace.DecidedByJevGate || rec.Judgment != record.Judgment {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if asked.asked(intake.TriggerRatified) {
		t.Fatal("a gate Jev decides today was judged a second time")
	}
}

func TestAScriptedDepartureAfterTheDebateConcludesAsksForAnotherRound(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{reply: func(ask jevfixture.Ask) jevfixture.Reply {
		if ask.Trigger == intake.TriggerShedConcluded {
			return jevfixture.Departs(ask, "debate_round", 0.96, 0.96)
		}
		return jevfixture.DefaultFits(ask)
	}}
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := journey.NewDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *journey.Options) { o.JevProvider = p.Factory() })
	defer f.Stop(t)

	stream := f.ReachJevPacket(t)
	more := f.AwaitSubjectMove(t, stream, intake.OwnerSubject, "more-1")
	if more.DecidedBy != trace.DecidedByJev || more.DefaultTo != "wait" || more.Judgment == "" {
		t.Fatalf("the departure %+v", more)
	}
	rec, ok := f.RouteOf(t, stream, intake.TriggerShedConcluded)
	if !ok || rec.Outcome != router.OutcomeDeparted || rec.Target != "debate_round" || rec.Default != "wait" || rec.Judgment != more.Judgment {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if docs := f.Documents(t, stream, shed.MoreDocumentID(1)); len(docs) == 0 {
		t.Fatalf("no request for another round is recorded")
	}
	f.AwaitShed(t, stream, "round-2", "failed-1")
}

func TestAScriptedAnswerNamingAnOwnerGateIsNeverCommitted(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{reply: func(ask jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(ask, "ratified", 0.99, 0.99)
	}}
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := journey.NewDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *journey.Options) { o.JevProvider = p.Factory() })
	defer f.Stop(t)

	stream := f.ReachJevPacket(t)
	f.S.Until(t, "the concluded debate to be routed", func() bool {
		_, ok := f.RouteOf(t, stream, intake.TriggerShedConcluded)
		return ok
	})
	rec, _ := f.RouteOf(t, stream, intake.TriggerShedConcluded)
	if rec.Target == "ratified" && rec.State == router.StateCommitted {
		t.Fatalf("a scripted answer committed an owner gate: %+v", rec)
	}
	for _, tr := range f.Transitions(t, stream) {
		if tr.To == "ratified-1" || tr.Subject == trace.FeatureSubject && tr.To == intake.RatifiedState {
			t.Fatalf("the workstream was ratified by a scripted answer: %+v", tr)
		}
	}
	for _, r := range p.Requests() {
		if _, ok := r.Questions[jev.QuestionFit]; !ok {
			continue
		}
		for _, o := range r.Questions[jev.QuestionPickA].Options {
			if o.Name == "ratified" {
				t.Fatalf("an owner gate was offered to Jev: %+v", r.Questions[jev.QuestionPickA].Options)
			}
		}
	}
}
