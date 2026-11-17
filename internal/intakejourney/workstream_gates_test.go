package intakejourney

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// routeRecords returns the route journal of the workstream's exits, the latest
// revision of each.
func (f *architectFixture) routeRecords(t *testing.T, stream config.WorkstreamID) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if _, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
		}
		if d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []router.Record
	for _, id := range order {
		var rec router.Record
		must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
		out = append(out, rec)
	}
	return out
}

func (f *architectFixture) routeOf(t *testing.T, stream config.WorkstreamID, trigger string) (router.Record, bool) {
	t.Helper()
	for _, rec := range f.routeRecords(t, stream) {
		if rec.Trigger == trigger {
			return rec, true
		}
	}
	return router.Record{}, false
}

// awaitSubjectMove waits for a transition of the subject into the value and
// returns it.
func (f *architectFixture) awaitSubjectMove(t *testing.T, stream config.WorkstreamID, subject, to string) trace.Transition {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		for _, tr := range f.transitions(t, stream) {
			if tr.Subject == subject && tr.To == to {
				return tr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s never moved %s to %s", stream, subject, to)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// triggersAsked records the trigger of every decide-transition request.
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

// The owner's ratification is a pinned move: the router enters ratified-<round>
// with the ratification file, names the owner as the decider and its authority,
// and sends Jev no decide-transition request for it.
func TestTheOwnersRatificationIsAPinnedMove(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{}
	// A Jev too unsure to decide the gate leaves the card to the owner.
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := newJevJourney(t, p, true)
	defer f.stop(t)
	ctx := context.Background()

	stream := f.reachJevPacket(t)
	f.awaitPacket(t, stream, "ratify: no objection stands")
	if _, err := f.c.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.scenario().AwaitFeature(t, stream, intake.BuildingState)

	ratified := f.awaitSubjectMove(t, stream, intake.OwnerSubject, "ratified-1")
	if ratified.DecidedBy != trace.DecidedByOwner || ratified.Judgment != "" || ratified.DefaultTo != "ratified-1" || ratified.Departed() || ratified.Actor != intake.OwnerActor {
		t.Fatalf("the owner's ratification %+v", ratified)
	}
	if record := f.ratification(t, stream, 1); record.DecidedBy == trace.DecidedByJev || record.Judgment != "" {
		t.Fatalf("the ratification file %+v", record)
	}
	rec, ok := f.routeOf(t, stream, intake.TriggerRatified)
	if !ok || rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.State != router.StateCommitted || rec.Target != "ratified" {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if asked.asked(intake.TriggerRatified) {
		t.Fatal("Jev was asked about an owner's ratification")
	}
}

// One of Jev's own gate decisions is applied the same way: ratified-<round> is
// entered as a pinned move whose authority is Jev's gate decision, with the
// judgment recorded, and no decide-transition request is sent for it.
func TestAJevGateRatificationIsAPinnedMoveWithItsJudgment(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{}
	p := &jevfixture.Provider{Confidence: 0.97, Transition: asked.script}
	f := newJevJourney(t, p, true)
	defer f.stop(t)

	stream := f.reachJevPacket(t)
	f.scenario().AwaitFeature(t, stream, intake.BuildingState)

	ratified := f.awaitSubjectMove(t, stream, intake.OwnerSubject, "ratified-1")
	docs := f.documents(t, stream, shed.RatificationDocumentID(1))
	if len(docs) != 1 {
		t.Fatalf("ratification records: %+v", docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	must(t, err)
	if ratified.DecidedBy != trace.DecidedByJevGate || ratified.Judgment == "" || ratified.Judgment != record.Judgment || ratified.DefaultTo != "ratified-1" || ratified.Departed() || ratified.Actor != trace.JevActor {
		t.Fatalf("Jev's ratification %+v, file %+v", ratified, record)
	}
	rec, ok := f.routeOf(t, stream, intake.TriggerRatified)
	if !ok || rec.Authority != router.AuthorityJevGate || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.DecidedBy != trace.DecidedByJevGate || rec.Judgment != record.Judgment {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if asked.asked(intake.TriggerRatified) {
		t.Fatal("a gate Jev decides today was judged a second time")
	}
}

// A scripted departure after the debate concluded asks for another round when
// it clears the strict bar: the owner subject enters more-<round> with the
// request the owner's command records, Jev is named its decider, and the
// departure is recorded; the next round then runs.
func TestAScriptedDepartureAfterTheDebateConcludesAsksForAnotherRound(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{reply: func(ask jevfixture.Ask) jevfixture.Reply {
		if ask.Trigger == intake.TriggerShedConcluded {
			return jevfixture.Departs(ask, "debate_round", 0.96, 0.96)
		}
		return jevfixture.DefaultFits(ask)
	}}
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := newDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *Options) { o.JevProvider = p.Factory() })
	defer f.stop(t)

	stream := f.reachJevPacket(t)
	more := f.awaitSubjectMove(t, stream, intake.OwnerSubject, "more-1")
	if more.DecidedBy != trace.DecidedByJev || more.DefaultTo != "wait" || more.Judgment == "" {
		t.Fatalf("the departure %+v", more)
	}
	rec, ok := f.routeOf(t, stream, intake.TriggerShedConcluded)
	if !ok || rec.Outcome != router.OutcomeDeparted || rec.Target != "debate_round" || rec.Default != "wait" || rec.Judgment != more.Judgment {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if docs := f.documents(t, stream, shed.MoreDocumentID(1)); len(docs) == 0 {
		t.Fatalf("no request for another round is recorded")
	}
	f.awaitShed(t, stream, "round-2", "failed-1")
}

// A scripted decide-transition answer that names an owner gate is never
// committed: the gate is not among the targets the router offers, so the
// default stands (or the exit is held) and the workstream is not ratified.
func TestAScriptedAnswerNamingAnOwnerGateIsNeverCommitted(t *testing.T) {
	t.Parallel()
	asked := &triggersAsked{reply: func(ask jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(ask, "ratified", 0.99, 0.99)
	}}
	p := &jevfixture.Provider{Confidence: 0.1, Transition: asked.script}
	f := newDebateFixtureWith(t, 1, 2, jevfixture.Config(jevfixture.Key(t, true)), func(o *Options) { o.JevProvider = p.Factory() })
	defer f.stop(t)

	stream := f.reachJevPacket(t)
	deadline := time.Now().Add(demoTimeout)
	for {
		if _, ok := f.routeOf(t, stream, intake.TriggerShedConcluded); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the concluded debate was never routed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec, _ := f.routeOf(t, stream, intake.TriggerShedConcluded)
	if rec.Target == "ratified" && rec.State == router.StateCommitted {
		t.Fatalf("a scripted answer committed an owner gate: %+v", rec)
	}
	for _, tr := range f.transitions(t, stream) {
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
