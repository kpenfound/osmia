package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAbandonedDependentIsReleasedFromItsEdgeWait(t *testing.T) {
	t.Parallel()
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait}, trace.EdgeHardBlock, &fixturekit.FakePulls{})
	defer f.stop(t)
	down, err := f.s.repository(f.project2)
	intakefixture.Must(t, err)
	waiting, err := lifecycle.EdgeWaiting(down, f.stream2)
	intakefixture.Must(t, err)
	if !waiting {
		t.Fatal("the dependent does not wait on its hard_block edge")
	}
	operation := func(action string) coreadapter.Operation {
		return coreadapter.Operation{ID: "operation-" + action, Boundary: coreadapter.RepositoryBoundary, Action: action}
	}
	released := []string{"land", "rebase", "publish", lifecycle.PublishUpstreamAction, lifecycle.DriftAction}
	hold := f.s.holding(down)
	for _, action := range released {
		if !hold(f.stream2, operation(action)) {
			t.Fatalf("the %s operation of a dependent waiting on its edge is not held", action)
		}
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-abandoned", Revision: 1, Project: f.project2, Workstream: f.stream2, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = down.SetFeatureState(context.Background(), h, intake.AbandonedState, "no longer needed")
	intakefixture.Must(t, err)
	if still, err := lifecycle.EdgeWaiting(down, f.stream2); err != nil || !still {
		t.Fatalf("abandoning changed the edge wait: %v, %v", still, err)
	}
	for _, action := range released {
		if hold(f.stream2, operation(action)) {
			t.Fatalf("the %s operation of the abandoned dependent is still held by its edge wait", action)
		}
	}
}
