package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAbandonedDependentIsReleasedFromItsEdgeWait(t *testing.T) {
	t.Parallel()
	f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait}, trace.EdgeHardBlock, &fixturekit.FakePulls{})
	defer f.Stop(t)
	down, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	waiting, err := lifecycle.EdgeWaiting(down, f.Stream2)
	fixturekit.Must(t, err)
	if !waiting {
		t.Fatal("the dependent does not wait on its hard_block edge")
	}
	operation := func(action string) coreadapter.Operation {
		return coreadapter.Operation{ID: "operation-" + action, Boundary: coreadapter.RepositoryBoundary, Action: action}
	}
	released := []string{"land", "rebase", "publish", lifecycle.PublishUpstreamAction, lifecycle.DriftAction}
	hold := f.S.Holding(down)
	for _, action := range released {
		if !hold(f.Stream2, operation(action)) {
			t.Fatalf("the %s operation of a dependent waiting on its edge is not held", action)
		}
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-abandoned", Revision: 1, Project: f.Project2, Workstream: f.Stream2, At: f.S.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = down.SetFeatureState(context.Background(), h, intake.AbandonedState, "no longer needed")
	fixturekit.Must(t, err)
	if still, err := lifecycle.EdgeWaiting(down, f.Stream2); err != nil || !still {
		t.Fatalf("abandoning changed the edge wait: %v, %v", still, err)
	}
	for _, action := range released {
		if hold(f.Stream2, operation(action)) {
			t.Fatalf("the %s operation of the abandoned dependent is still held by its edge wait", action)
		}
	}
}
