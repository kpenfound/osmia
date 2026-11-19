package delivery

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// moveBase lands the fixture's workstream once while the base branch has moved
// since the review, so the landing ends base-moved, and returns the commit the
// base moved to.
func (f *landFixture) moveBase(decider LandDecider) string {
	f.t.Helper()
	moved := f.advanceUpstream("someone-else.txt")
	if result := f.land(decider); result.Outcome != LandingBaseMoved {
		f.t.Fatalf("the landing is %+v, want the base moved", result)
	}
	return moved
}

func (f *landFixture) route() LandingRoute {
	f.t.Helper()
	route, found, err := ReadLandingRoute(f.repo, stream)
	fixturekit.Must(f.t, err)
	if !found {
		f.t.Fatal("no landing route is recorded")
	}
	return route
}

// requireDropped checks that the retry dropped the route for a reason naming
// want, landed nothing and asked for no further attempt.
func (f *landFixture) requireDropped(want string) {
	f.t.Helper()
	route := f.route()
	if route.Status != LandingRouteDropped || !strings.Contains(route.Reason, want) {
		f.t.Fatalf("the route is %+v, want it dropped for %q", route, want)
	}
	if _, pending, err := PendingLandingRoute(f.repo, stream); err != nil || pending {
		f.t.Fatalf("a dropped route is still pending: %v %v", pending, err)
	}
	if f.landingAttempts() != 1 || f.feature() != AssembledState || f.pulls.Readies != 0 {
		f.t.Fatalf("the dropped route left %d attempts, the workstream %s and %d pull requests ready", f.landingAttempts(), f.feature(), f.pulls.Readies)
	}
	// Later passes leave it dropped.
	f.retry()
	if f.landingAttempts() != 1 || f.route().Status != LandingRouteDropped {
		f.t.Fatal("a pass after the route was dropped acted")
	}
}

// A binding decision of Jev's to land is kept across the move of the base and
// carried out on the rebased commit under Jev's authority, with the judgment it
// rests on.
func TestAJevDecisionToLandIsRetriedOnTheRebasedCommitUnderJev(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	moved := f.moveBase(byJev())
	route := f.route()
	if route.Status != LandingRoutePending || route.Decider != byJev() {
		t.Fatalf("the route is %+v", route)
	}
	_, signed := f.rebased(2, moved)
	f.retry()
	if got := f.upstreamMain(); got != signed {
		t.Fatalf("the base branch is at %s, want the rebased commit %s", got, signed)
	}
	landing, found, err := ReadDeliveryLanding(f.repo, stream)
	fixturekit.Must(t, err)
	if !found || landing.DecidedBy != trace.DecidedByJev || landing.Judgment != judgmentKey || landing.Commit != signed {
		t.Fatalf("the landing record is %+v", landing)
	}
	delivered := f.transitions(DeliveredState)
	if len(delivered) != 1 || delivered[0].DecidedBy != trace.DecidedByJevGate {
		t.Fatalf("the delivered transitions are %+v", delivered)
	}
	if f.landingAttempts() != 2 || f.stepCount("land-recorded") != 1 {
		t.Fatalf("%d attempts, %d pushes of the base branch", f.landingAttempts(), f.stepCount("land-recorded"))
	}
}

// The rebase brings a new push, but until its checks are recorded the route
// waits; a pass never lands the commit that did not land.
func TestAPendingRouteWaitsForTheChecksOfTheRebasedPush(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	moved := f.moveBase(byOwner())
	f.rebasedWith(2, moved, "none")
	f.retry()
	if _, pending, err := PendingLandingRoute(f.repo, stream); err != nil || !pending {
		t.Fatalf("the route is not pending: %v %v", pending, err)
	}
	if f.upstreamMain() != moved || f.landingAttempts() != 1 || f.feature() != AssembledState {
		t.Fatalf("the pass acted without checks: %s, %d attempts, %s", f.upstreamMain(), f.landingAttempts(), f.feature())
	}
}

// Checks that failed on the rebased push are the existing repair path's: the
// route is dropped, nothing lands and the pull request is not marked ready.
func TestAPendingRouteIsDroppedWhenChecksFailOnTheRebasedPush(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	moved := f.moveBase(byOwner())
	f.rebasedWith(2, moved, "failed")
	f.retry()
	f.requireDropped("checks did not pass")
	if f.upstreamMain() != moved {
		t.Fatal("the base branch moved")
	}
}

// A new final review that leaves blocking findings to the owner never lands
// automatically: the earlier acceptance is not carried over, the route is
// dropped and the delivery is the owner's.
func TestAPendingRouteIsDroppedWhenTheNewFinalReviewLeavesBlockingFindings(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	moved := f.moveBase(byOwner())
	f.findings = []FinalFinding{{Severity: FindingBlocking, Finding: "the rebase broke the resume"}}
	f.rebased(2, moved)
	f.retry()
	f.requireDropped("blocking findings")
	if f.upstreamMain() != moved {
		t.Fatal("the base branch moved")
	}
}

// Every gate is rechecked on the retry: a dependency that waits to merge, or a
// project that no longer lands, drops the route.
func TestAPendingRouteIsDroppedWhenALandingGateNoLongerHolds(t *testing.T) {
	t.Parallel()
	t.Run("a hard-block dependency waits", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		moved := f.moveBase(byOwner())
		f.rebased(2, moved)
		f.options.edgeWait = "waiting for acme/other#3 to merge"
		f.retry()
		f.requireDropped("acme/other#3")
	})
	t.Run("a merge-order dependency waits", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		moved := f.moveBase(byJev())
		f.rebased(2, moved)
		f.options.mergeOrder = "acme/other#3"
		f.retry()
		f.requireDropped("acme/other#3")
	})
	t.Run("the project no longer lands", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		moved := f.moveBase(byOwner())
		f.rebased(2, moved)
		f.cfg.Project.Delivery = "pull-request"
		f.retry()
		f.requireDropped("delivers by pull request")
	})
}

// A retry whose base moved again asks for another rebase and keeps the
// decision, and each attempt lands a different, newly pushed commit: nothing is
// pushed to the base branch twice.
func TestARetryWhoseBaseMovedAgainKeepsTheDecisionAndLandsOnce(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	moved := f.moveBase(byJev())
	f.rebased(2, moved)
	again := f.advanceUpstream("another.txt")
	f.retry()
	route := f.route()
	if route.Status != LandingRoutePending || route.Landing != 2 || route.Push != 2 || route.Decider != byJev() {
		t.Fatalf("the route is %+v", route)
	}
	if f.upstreamMain() != again || f.rebases != 2 || f.landingAttempts() != 2 || f.stepCount("land-pushed") != 0 {
		t.Fatalf("base at %s, %d rebases, %d attempts, %d pushes", f.upstreamMain(), f.rebases, f.landingAttempts(), f.stepCount("land-pushed"))
	}
	// The same push is not landed again.
	f.retry()
	if f.landingAttempts() != 2 {
		t.Fatalf("%d attempts", f.landingAttempts())
	}
	_, signed := f.rebased(3, again)
	f.retry()
	if f.upstreamMain() != signed || f.subject(LandingSubject) != "landed-3" || f.stepCount("land-pushed") != 1 {
		t.Fatalf("base at %s, landing %s, %d pushes", f.upstreamMain(), f.subject(LandingSubject), f.stepCount("land-pushed"))
	}
}
