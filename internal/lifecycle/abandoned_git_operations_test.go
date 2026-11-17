package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// A dependent with an unresolved hard_block edge holds its landings, rebases
// and publications while the edge waits. Once the dependent is abandoned they
// are released, so they reach a result and no longer defer its archive
// cleanup; its drift rebase is released the same way, and operations that the
// edge wait does not govern are untouched.
func TestAbandonedDependentIsReleasedFromItsEdgeWait(t *testing.T) {
	t.Parallel()
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait}, trace.EdgeHardBlock, &fakePulls{})
	defer f.stop(t)
	down, err := f.s.repository(f.project2)
	must(t, err)
	waiting, err := edgeWaiting(down, f.stream2)
	must(t, err)
	if !waiting {
		t.Fatal("the dependent does not wait on its hard_block edge")
	}
	operation := func(action string) coreadapter.Operation {
		return coreadapter.Operation{ID: "operation-" + action, Boundary: coreadapter.RepositoryBoundary, Action: action}
	}
	released := []string{"land", "rebase", "publish", publishUpstreamAction, DriftAction}
	hold := f.s.holding(down)
	for _, action := range released {
		if !hold(f.stream2, operation(action)) {
			t.Fatalf("the %s operation of a dependent waiting on its edge is not held", action)
		}
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-abandoned", Revision: 1, Project: f.project2, Workstream: f.stream2, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = down.SetFeatureState(context.Background(), h, AbandonedState, "no longer needed")
	must(t, err)
	if still, err := edgeWaiting(down, f.stream2); err != nil || !still {
		t.Fatalf("abandoning changed the edge wait: %v, %v", still, err)
	}
	for _, action := range released {
		if hold(f.stream2, operation(action)) {
			t.Fatalf("the %s operation of the abandoned dependent is still held by its edge wait", action)
		}
	}
}
