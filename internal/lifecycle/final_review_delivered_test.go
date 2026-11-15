package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// The final reviewer's pass reads a workstream assembled, and its publication
// can deliver it before the pass asks for the review. A delivered workstream
// with no maintenance pending is asked for no review.
func TestFinalReviewOfAWorkstreamDeliveredDuringThePassAsksForNothing(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	p.approve(t, nil)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	feature, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	must(t, err)
	if feature.Value != DeliveredState {
		t.Fatalf("feature is %s, not delivered", feature.Value)
	}
	pending, err := upstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
	must(t, err)
	if pending {
		t.Fatal("a fresh delivery has maintenance pending")
	}
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.request(ctx, p.stream))
	ops, err := p.repository.Operations(p.stream)
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action == deliveryReviewAction {
			t.Fatalf("maintenance review asked for with nothing pending: %s", o.Operation.Input)
		}
	}
}
