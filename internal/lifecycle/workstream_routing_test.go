package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestTheOwnersAbandonmentIsAPinnedMove(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "route-abandon", "")
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.S).SetJudge(jevfixture.TransitionJudge(f.S.Now, p))
	tool := managerMoveTool(t, f, repository, stream, "message_1", true)
	if out := intakefixture.Handle(t, tool, `{"to":"abandoned","note":"Stop this one.","owner_decided":true}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("the relayed abandonment: %s", out)
	}
	abandon := intakefixture.TransitionByID(t, repository, stream, intake.AbandonTransition)
	if abandon.To != intake.AbandonedState || abandon.DecidedBy != trace.DecidedByOwner || abandon.DefaultTo != intake.AbandonedState || abandon.Judgment != "" || abandon.Departed() || abandon.Actor != intake.OwnerActor {
		t.Fatalf("the abandonment %+v", abandon)
	}
	rec, ok := journey.LookupRoute(t, repository, stream, "ws.abandon")
	if !ok || rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if n := len(p.Requests()); n != 0 {
		t.Fatalf("Jev was asked %d times about an owner's decision", n)
	}
}

func TestTheOwnersWorkstreamMoveIsAPinnedMove(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, "route-owner-move", "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.S).SetJudge(jevfixture.TransitionJudge(f.S.Now, p))
	journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	out, apiFault := f.S.Lifecycle().OwnerMoveWorkstream(t.Context(), string(stream), api.WorkstreamMoveRequest{To: intake.BuildingState, Note: "Reopen it."})
	if apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	if out.State != intake.BuildingState || out.Move == nil || out.Move.By != "owner" {
		t.Fatalf("the move %+v", out)
	}
	var feature trace.Transition
	for _, tr := range transitions(t, repository, stream) {
		if tr.Subject == trace.FeatureSubject && tr.To == intake.BuildingState && tr.ID == lifecycle.WorkstreamMoveDocument(1)+"-"+intake.BuildingState {
			feature = tr
		}
	}
	if feature.DecidedBy != trace.DecidedByOwner || feature.DefaultTo != intake.BuildingState || feature.Actor != intake.OwnerActor {
		t.Fatalf("the feature's transition %+v", feature)
	}
	rec, ok := journey.LookupRoute(t, repository, stream, "ws.move_owner")
	if !ok || rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
	if n := len(p.Requests()); n != 0 {
		t.Fatalf("Jev was asked %d times about an owner's decision", n)
	}
}

func TestAManagerProposalToAGateLeavesAPendingOwnerDecision(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, "route-manager-gate", "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(ask, "abandoned", 0.99, 0.99)
	}}
	journey.InspectForTest(f.S).SetJudge(jevfixture.TransitionJudge(f.S.Now, p))
	journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	tool := managerMoveTool(t, f, repository, stream, "events_1", false)

	for _, to := range []string{intake.AbandonedState, intake.RatifiedState} {
		out := intakefixture.Handle(t, tool, `{"to":"`+to+`","note":"Nothing else is moving it."}`)
		if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "the owner's decision") || strings.Contains(out, "say") {
			t.Fatalf("the manager proposal to %s: %s", to, out)
		}
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("a proposal moved the workstream: %+v %v", feature, err)
	}
	for _, tr := range transitions(t, repository, stream) {
		if tr.Subject == trace.FeatureSubject && (tr.To == intake.AbandonedState || tr.To == intake.RatifiedState) {
			t.Fatalf("a proposal committed an owner gate: %+v", tr)
		}
	}
	pending, err := router.Unfinished(repository, stream)
	fixturekit.Must(t, err)
	targets := map[string]router.Record{}
	for _, rec := range pending {
		if rec.Trigger == "ws.move_proposal" {
			targets[rec.Target] = rec
		}
	}
	for to, card := range map[string]string{intake.AbandonedState: "the recovery card", intake.RatifiedState: "the ratification card"} {
		rec, ok := targets[to]
		if !ok || rec.State != router.StateOwner || rec.Hold != router.HoldGate || rec.Outcome != router.OutcomeHeld || rec.JevRequested || rec.Authority != "" {
			t.Fatalf("pending decision for %s: %+v (found %v)", to, rec, ok)
		}
		notices := 0
		outbox, err := repository.Outbox(stream)
		fixturekit.Must(t, err)
		for _, e := range outbox {
			if e.Event.Kind == trace.NoticeKind && strings.HasPrefix(e.TransitionID, "route-") && strings.Contains(e.Event.Body, card) && strings.Contains(e.Event.Body, to) {
				notices++
			}
		}
		if notices != 1 {
			t.Fatalf("%d notices name %s for %s, want one", notices, card, to)
		}
	}
	if n := len(p.Requests()); n != 0 {
		t.Fatalf("Jev was asked %d times about an owner gate", n)
	}
}

func TestAManagerMoveToBuildingIsPinnedAndCommitted(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, "route-manager-building", "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.S).SetJudge(jevfixture.TransitionJudge(f.S.Now, p))
	journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	tool := managerMoveTool(t, f, repository, stream, "events_2", false)
	if out := intakefixture.Handle(t, tool, `{"to":"building","note":"Reopen it so a revision can repair the gap."}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"building"`) {
		t.Fatalf("move_workstream to building: %s", out)
	}
	var feature trace.Transition
	for _, tr := range transitions(t, repository, stream) {
		if tr.Subject == trace.FeatureSubject && tr.To == intake.BuildingState && strings.HasPrefix(tr.ID, lifecycle.WorkstreamMoveDocument(1)) {
			feature = tr
		}
	}
	if feature.DecidedBy != trace.DecidedByManager || feature.DefaultTo != intake.BuildingState || feature.Departed() {
		t.Fatalf("the feature's transition %+v", feature)
	}
	rec, ok := journey.LookupRoute(t, repository, stream, "ws.move_proposal")
	if !ok || rec.Outcome != router.OutcomeFollowed || rec.State != router.StateCommitted || rec.Default != intake.BuildingState {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
}

func transitions(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	all, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	return all
}
