package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFinalReviewOfAWorkstreamDeliveredDuringThePassAsksForNothing(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	p.approve(t, nil)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	feature, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	intakefixture.Must(t, err)
	if feature.Value != intake.DeliveredState {
		t.Fatalf("feature is %s, not delivered", feature.Value)
	}
	pending, err := lifecycle.UpstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
	intakefixture.Must(t, err)
	if pending {
		t.Fatal("a fresh delivery has maintenance pending")
	}
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	intakefixture.Must(t, reviewer.request(ctx, p.stream))
	ops, err := p.repository.Operations(p.stream)
	intakefixture.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == lifecycle.DeliveryReviewAction {
			t.Fatalf("maintenance review asked for with nothing pending: %s", o.Operation.Input)
		}
	}
}
