package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAChiefMoveReturnsAReopenedWorkstreamToDelivered(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "squash")
	ctx := context.Background()
	p.Approve(t, nil)
	if result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher()); result.Outcome != "succeeded" || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("the first publication %+v", result)
	}
	p.Pulls.SyncHead = true
	if _, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: intake.BuildingState, Note: "Repair it on this pull request."}); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	if p.Feature(t) != intake.BuildingState {
		t.Fatalf("the reopened workstream is %s", p.Feature(t))
	}
	jev := &jevfixture.Transitions{}
	journey.InspectForTest(p.S).SetRouterJudge(jevfixture.TransitionJudge(p.S.Now, jev))
	tool := chiefMoveTool(t, p.ShedFixture, p.Repository, p.Stream, "events_1", false)

	out := intakefixture.Handle(t, tool, `{"to":"delivered","note":"Nothing needed repair; the pull request is as published."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"delivered"`) {
		t.Fatalf("the chief's move: %s", out)
	}
	moved := lastOn(t, p.Repository, p.Stream, trace.FeatureSubject)
	if moved.To != intake.DeliveredState || moved.DecidedBy != trace.DecidedByManager || moved.Judgment != "" || moved.Departed() {
		t.Fatalf("the transition %+v", moved)
	}
	if n := len(jev.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a chief move", n)
	}
	records := routeFor(t, p.Repository, p.Stream, "ws.move_proposal")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].Authority != router.AuthorityChief {
		t.Fatalf("route %+v", records)
	}
}
