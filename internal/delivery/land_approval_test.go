package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestApproveAndLandIsRefusedOnAProjectThatDeliversByPullRequest(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t, landOptions{delivery: config.DeliveryPullRequest})
	_, api := f.controller().ApproveAndLand(context.Background(), string(stream), DeliveryLandDecision{Review: 1})
	if api == nil || api.Code != Conflict || !strings.Contains(api.Message, `delivery is "pull-request"`) || !strings.Contains(api.Message, `set delivery to "jev"`) {
		t.Fatalf("approving to land gave %+v, want a conflict naming the delivery setting", api)
	}
	if f.documents(DeliveryDocument) != 0 || f.upstreamMain() != f.base {
		t.Fatal("a refused landing recorded an approval or pushed")
	}
}

func TestApproveAndLandIsRefusedWhileADependencyWaitsToMerge(t *testing.T) {
	t.Parallel()
	t.Run("hard block", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t, landOptions{edgeWait: "waiting for acme/other#3 to merge"})
		_, api := f.controller().ApproveAndLand(context.Background(), string(stream), DeliveryLandDecision{Review: 1})
		if api == nil || api.Code != Conflict || !strings.Contains(api.Message, "acme/other#3") {
			t.Fatalf("approving to land gave %+v, want a conflict naming the dependency", api)
		}
		f.nothingLanded()
	})
	t.Run("merge order", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t, landOptions{mergeOrder: "acme/other#3"})
		result, err := f.controller().LandWorkstream(context.Background(), f.repo, stream, LandRequest{AcceptBlocking: true})
		fixturekit.Must(t, err)
		if result.Outcome != LandingRefusedBeforePush || !strings.Contains(result.Reason, "acme/other#3 has not merged yet") {
			t.Fatalf("the landing is %+v, want it refused for the merge-order dependency", result)
		}
		f.nothingLanded()
	})
}

func (f *landFixture) nothingLanded() {
	f.t.Helper()
	if f.documents(DeliveryDocument) != 0 || f.upstreamMain() != f.base || f.feature() != AssembledState {
		f.t.Fatalf("a refused landing recorded %d approvals, left the base at %s (was %s) and the workstream %s", f.documents(DeliveryDocument), f.upstreamMain(), f.base, f.feature())
	}
	if ops, err := f.repo.Operations(stream); err != nil || len(ops) != 0 {
		f.t.Fatalf("a refused landing recorded operations %v %v", ops, err)
	}
}
