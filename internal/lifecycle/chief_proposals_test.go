package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// A reopened workstream returns to delivered only by a departure: the chief of
// staff does not make that move by default, so the default leaves the
// workstream where it is and the result says so, and a Jev that finds the
// evidence supports the move returns it at the strict bar and records the
// departure.
func TestChiefProposalToReturnToDeliveredIsMadeOnlyByADeparture(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	ctx := context.Background()
	p.approve(t, nil)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" || p.feature(t) != DeliveredState {
		t.Fatalf("the first publication %+v", result)
	}
	p.pulls.SyncHead = true
	if _, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: BuildingState, Note: "Repair it on this pull request."}); api != nil {
		t.Fatal(api.Message)
	}
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.Pass(ctx))
	if p.feature(t) != BuildingState {
		t.Fatalf("the reopened workstream is %s", p.feature(t))
	}
	jev := &jevfixture.Transitions{}
	routeJev(p.s, jev)
	tool := chiefMoveTool(t, p.shedFixture, p.repository, p.stream, "events_1", false)

	out := callMove(t, tool, `{"to":"delivered","note":"Nothing needed repair; the pull request is as published."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "stays building") || strings.Contains(out, "owner_decided") {
		t.Fatalf("the proposal that followed the default: %s", out)
	}
	if p.feature(t) != BuildingState {
		t.Fatalf("a followed default moved the workstream to %s", p.feature(t))
	}
	records := routeFor(t, p.repository, p.stream, "ws.move_proposal")
	if len(records) != 1 || records[0].Default != lifecycle.NoMoveTarget || records[0].Outcome != router.OutcomeFollowed || records[0].Proposed != DeliveredState {
		t.Fatalf("route %+v", records)
	}

	jev.Script = departTo("delivered", 0.96).Script
	out = callMove(t, tool, `{"to":"delivered","note":"The repair was not needed after all; the branch is what the pull request carries."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"delivered"`) {
		t.Fatalf("the departure: %s", out)
	}
	moved := lastOn(t, p.repository, p.stream, trace.FeatureSubject)
	if moved.To != DeliveredState || moved.DecidedBy != trace.DecidedByJev || moved.DefaultTo != lifecycle.NoMoveTarget || !moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the transition %+v", moved)
	}
	records = routeFor(t, p.repository, p.stream, "ws.move_proposal")
	if len(records) != 2 || records[1].Outcome != router.OutcomeDeparted || records[1].Target != DeliveredState {
		t.Fatalf("route %+v", records)
	}
}

// A Jev that cannot choose between the targets holds the proposal: the
// workstream stays, the chief of staff is told the move is held, and the result
// does not ask the owner for anything.
func TestAHeldChiefWorkstreamProposalSaysSoAndMovesNothing(t *testing.T) {
	t.Parallel()
	f, repository, stream, tool := atTheFinalRoundLimit(t, "limit-held")
	routeJev(f.s, &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.Departs(a, "building", 0.9, 0.9)
		r.PickB = lifecycle.NoMoveTarget
		return r
	}})
	out := callMove(t, tool, `{"to":"building","note":"Reopen it."}`)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "held") || strings.Contains(out, "owner_decided") {
		t.Fatalf("the held proposal: %s", out)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
}
