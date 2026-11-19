package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// No delivery-route gate stands, so Jev is asked nothing, for a delivery whose
// project delivers by pull request or through a fork, whose pushed commit has
// no passing checks, or that waits on a hard-block or merge-order dependency.
func TestNoDeliveryRouteGateStandsForADeliveryThatIsNotClean(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		opts landOptions
		fork string
	}{
		{name: "a project that delivers by pull request", opts: landOptions{delivery: config.DeliveryPullRequest}},
		{name: "a delivery through a fork", fork: "someone/widgets"},
		{name: "checks that failed", opts: landOptions{checks: "failed"}},
		{name: "no check result", opts: landOptions{checks: "none"}},
		{name: "checks that passed on another commit", opts: landOptions{checks: "other"}},
		{name: "a hard-block dependency waiting to merge", opts: landOptions{edgeWait: "waiting for acme/other#3 to merge"}},
		{name: "a merge-order dependency waiting to merge", opts: landOptions{mergeOrder: "acme/other#3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t, c.opts)
			f.cfg.Project.Fork = c.fork
			g, err := f.controller().DeliveryRouteGate(context.Background(), f.repo, stream)
			fixturekit.Must(t, err)
			if g != nil {
				t.Fatalf("a delivery that is not clean has the gate %+v", g)
			}
		})
	}
}

// The service refuses to apply a pick made for a delivery that is no longer the
// workstream's current one, or that is not a route, and records nothing and
// pushes nothing.
func TestAPickForAnotherDeliveryIsRefusedWithNothingPushed(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	res := jev.Result{Status: jev.Binding, Pick: RouteLand, Judgment: judgmentKey}
	c := f.controller()
	push, found, err := CurrentPush(f.repo, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("no push is recorded")
	}
	other := jev.DeliveryRouteKey(stream, push.Review+1, push.Commit)
	for _, pick := range []string{RouteLand, RoutePullRequest} {
		res.Pick = pick
		if api := c.applyDeliveryRoute(context.Background(), f.repo, stream, other, res); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "no longer") {
			t.Fatalf("applying %s for another delivery gave %+v", pick, api)
		}
	}
	res.Pick = "merge"
	if api := c.applyDeliveryRoute(context.Background(), f.repo, stream, deliveryRouteKey(stream, push), res); api == nil || api.Code != Conflict {
		t.Fatalf("applying a pick that is no route gave %+v", api)
	}
	f.nothingLanded()
}

// A delivery whose project does not land is never given a route, even by a
// pick recorded for it.
func TestAPickIsRefusedOnAProjectThatDoesNotLand(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t, landOptions{delivery: config.DeliveryPullRequest})
	push, _, err := CurrentPush(f.repo, stream)
	fixturekit.Must(t, err)
	res := jev.Result{Status: jev.Binding, Pick: RoutePullRequest, Judgment: judgmentKey}
	if api := f.controller().applyDeliveryRoute(context.Background(), f.repo, stream, deliveryRouteKey(stream, push), res); api == nil || api.Code != Conflict {
		t.Fatalf("applying a pick on a project that does not land gave %+v", api)
	}
	f.nothingLanded()
}

// A landing Jev decided whose attempt ended without landing is a decision the
// service refused to carry out: the gate has none, and applying the pick again
// says why, so the pick is handed to the owner and no second landing is tried.
func TestAJevLandingThatEndedWithoutLandingIsRefusedNotRetried(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	push, _, err := CurrentPush(f.repo, stream)
	fixturekit.Must(t, err)
	decider := byJev()
	res := jev.Result{Status: jev.Binding, Pick: RouteLand, Judgment: decider.Judgment}
	c := f.controller()
	op, err := f.lander().Request(context.Background(), stream, LandInput{Landing: 1, Push: push.Number, Review: push.Review, Reviewed: push.Reviewed, Commit: push.Commit, Decider: decider})
	fixturekit.Must(t, err)
	run := &landingRun{Lander: f.lander(), cfg: f.cfg, stream: stream, op: op, in: LandInput{Landing: 1, Push: push.Number, Review: push.Review, Reviewed: push.Reviewed, Commit: push.Commit, Decider: decider}}
	_, err = run.fail(context.Background(), "a precondition no longer holds")
	fixturekit.Must(t, err)

	decision, err := c.deliveryRouteDecision(f.repo, stream, push)
	fixturekit.Must(t, err)
	if decision.Recorded {
		t.Fatalf("a landing that failed is a decision: %+v", decision)
	}
	api := c.applyDeliveryRoute(context.Background(), f.repo, stream, deliveryRouteKey(stream, push), res)
	if api == nil || api.Code != intake.Conflict || !strings.Contains(api.Message, "precondition no longer holds") {
		t.Fatalf("applying the pick again gave %+v", api)
	}
	if ops, err := f.repo.Operations(stream); err != nil || len(ops) != 1 {
		t.Fatalf("%d operations, %v; a second landing was tried", len(ops), err)
	}
	if f.upstreamMain() != f.base {
		t.Fatal("the base branch moved")
	}
}
