package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFinalReviewOfAWorkstreamDeliveredDuringThePassAsksForNothing(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	p.Approve(t, nil)
	if result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher()); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	feature, err := p.Repository.Workflow(p.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if feature.Value != intake.DeliveredState {
		t.Fatalf("feature is %s, not delivered", feature.Value)
	}
	pending, err := lifecycle.UpstreamDeliveryPending(p.Repository, p.S.Cfg, p.Stream)
	fixturekit.Must(t, err)
	if pending {
		t.Fatal("a fresh delivery has maintenance pending")
	}
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Request(ctx, p.Stream))
	ops, err := p.Repository.Operations(p.Stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == lifecycle.DeliveryReviewAction {
			t.Fatalf("maintenance review asked for with nothing pending: %s", o.Operation.Input)
		}
	}
}
