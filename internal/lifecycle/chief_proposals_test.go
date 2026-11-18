package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// The chief of staff's move of a reopened workstream back to delivered is its
// own decision: the router enters it after the hard prerequisites, asks Jev
// nothing and records the chief of staff as the decider.
func TestAChiefMoveReturnsAReopenedWorkstreamToDelivered(t *testing.T) {
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
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"delivered"`) {
		t.Fatalf("the chief's move: %s", out)
	}
	moved := lastOn(t, p.repository, p.stream, trace.FeatureSubject)
	if moved.To != DeliveredState || moved.DecidedBy != trace.DecidedByChief || moved.Judgment != "" || moved.Departed() {
		t.Fatalf("the transition %+v", moved)
	}
	if n := len(jev.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a chief move", n)
	}
	records := routeFor(t, p.repository, p.stream, "ws.move_proposal")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].Authority != router.AuthorityChief {
		t.Fatalf("route %+v", records)
	}
}
