package activity_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	activitylib "github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func floatPtr(v float64) *float64 { return &v }

func startHealthFixture(t *testing.T) (*service.Service, *service.Client, *fixturekit.FixedClock) {
	t.Helper()
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts := servicetest.Fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := servicetest.Start(t, opts)
	return s, c, clock
}

func TestHealthIsReadyWithNothingInFlight(t *testing.T) {
	t.Parallel()
	_, c, _ := startHealthFixture(t)
	ctx := context.Background()
	ready, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if !ready.API || !ready.Ready || ready.Backend != api.BackendReady || ready.Cause != "" {
		t.Fatalf("expected a ready backend with nothing in flight: %+v", ready)
	}
	activity, err := c.Activity(ctx)
	fixturekit.Must(t, err)
	if len(activity.Activity) != 0 {
		t.Fatalf("startup recovery left activity behind: %+v", activity.Activity)
	}
}

func TestHealthIsNotReadyWhileRecoveryRuns(t *testing.T) {
	t.Parallel()
	s, c, _ := startHealthFixture(t)
	recovering := s.StartActivity("health-recovery", api.ActivityRecovery, fixturekit.Project, "", "recovering interrupted workspace operations")
	notReady, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !notReady.API || notReady.Ready || notReady.Backend != api.BackendNotReady || notReady.Cause == "" {
		t.Fatalf("expected a not-ready backend while recovery is in progress: %+v", notReady)
	}
	recovering.Finish(nil)
	ready, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !ready.Ready || ready.Backend != api.BackendReady {
		t.Fatalf("expected a ready backend once recovery finished: %+v", ready)
	}
}

func TestHealthIsDegradedByAFailedOperationUntilItIsStartedAgain(t *testing.T) {
	t.Parallel()
	s, c, _ := startHealthFixture(t)
	failing := s.StartActivity("health-failure", api.ActivityGit, fixturekit.Project, fixturekit.Stream, "rebase")
	failing.Finish(errors.New("the upstream branch moved"))
	degraded, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !degraded.API || degraded.Ready || degraded.Backend != api.BackendDegraded ||
		!strings.Contains(degraded.Cause, "the upstream branch moved") {
		t.Fatalf("expected a degraded backend naming the failure: %+v", degraded)
	}
	s.StartActivity("health-failure", api.ActivityGit, fixturekit.Project, fixturekit.Stream, "rebase").Finish(nil)
	ready, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !ready.Ready || ready.Backend != api.BackendReady || ready.Cause != "" {
		t.Fatalf("expected a ready backend once the operation succeeded: %+v", ready)
	}
}

func TestHealthIsDegradedByAnOperationThatStoppedReportingProgress(t *testing.T) {
	t.Parallel()
	s, c, clock := startHealthFixture(t)
	stalled := s.StartActivity("health-stall", api.ActivityGit, fixturekit.Project, fixturekit.Stream, "cloning")
	stalled.Progress("cloning", nil)
	clock.Advance(activitylib.ActivityStallThreshold)
	degraded, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !degraded.API || degraded.Ready || degraded.Backend != api.BackendDegraded ||
		!strings.Contains(degraded.Cause, "cloning") {
		t.Fatalf("expected a degraded backend naming the stalled operation: %+v", degraded)
	}
	stalled.Finish(nil)
	ready, err := c.Health(context.Background())
	fixturekit.Must(t, err)
	if !ready.Ready || ready.Backend != api.BackendReady || ready.Cause != "" {
		t.Fatalf("expected a ready backend once the operation finished: %+v", ready)
	}
}

func TestHealthDoesNotDegradeOnAGitOperationThatNeverReportsProgress(t *testing.T) {
	t.Parallel()
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts := servicetest.Fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := servicetest.Start(t, opts)
	ctx := context.Background()

	running := s.StartActivity("long-git", api.ActivityGit, fixturekit.Project, fixturekit.Stream, "pushing")
	clock.Advance(10 * activitylib.ActivityStallThreshold)

	stillReady, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if !stillReady.API || !stillReady.Ready || stillReady.Backend != api.BackendReady || stillReady.Cause != "" {
		t.Fatalf("expected a ready backend while a Git operation that never reports progress is still running: %+v", stillReady)
	}

	running.Progress("pushing", nil)
	clock.Advance(activitylib.ActivityStallThreshold)
	degraded, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if !degraded.API || degraded.Ready || degraded.Backend != api.BackendDegraded || !strings.Contains(degraded.Cause, "pushing") {
		t.Fatalf("expected a degraded backend once the operation reported progress and then stalled: %+v", degraded)
	}
	running.Finish(nil)
}

func TestHealthDoesNotDegradeOnOrdinaryLongRunningTurnsAndChecks(t *testing.T) {
	t.Parallel()
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts := servicetest.Fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := servicetest.Start(t, opts)
	ctx := context.Background()

	turn := s.StartActivity("long-turn", api.ActivityAgentTurn, fixturekit.Project, fixturekit.Stream, "mason turn")
	check := s.StartActivity("long-check", api.ActivityCheck, fixturekit.Project, fixturekit.Stream, "running checks")
	clock.Advance(10 * activitylib.ActivityStallThreshold)

	stillReady, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if !stillReady.API || !stillReady.Ready || stillReady.Backend != api.BackendReady || stillReady.Cause != "" {
		t.Fatalf("expected a ready backend while a turn and a check are still running: %+v", stillReady)
	}

	turn.Finish(nil)
	check.Finish(nil)
}

func TestHealthStopsDegradingOnAFailureNothingRetries(t *testing.T) {
	t.Parallel()
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts := servicetest.Fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := servicetest.Start(t, opts)
	ctx := context.Background()

	abandoned := s.StartActivity("abandoned-check-run-1", api.ActivityCheck, fixturekit.Project, fixturekit.Stream, "running checks")
	abandoned.Finish(errors.New("unit workstream was abandoned"))

	degraded, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if !degraded.API || degraded.Ready || degraded.Backend != api.BackendDegraded {
		t.Fatalf("expected a degraded backend right after the unretried failure: %+v", degraded)
	}

	clock.Advance(activitylib.ActivityFailureExpiry - time.Second)
	stillDegraded, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if stillDegraded.Ready || stillDegraded.Backend != api.BackendDegraded {
		t.Fatalf("expected the failure to still degrade health just before it expires: %+v", stillDegraded)
	}

	clock.Advance(time.Second)
	ready, err := c.Health(ctx)
	fixturekit.Must(t, err)
	if !ready.API || !ready.Ready || ready.Backend != api.BackendReady || ready.Cause != "" {
		t.Fatalf("expected an unretried failure to stop degrading health once it expires: %+v", ready)
	}
	activity, err := c.Activity(ctx)
	fixturekit.Must(t, err)
	if len(activity.Activity) != 0 {
		t.Fatalf("expected the expired failure to drop out of the activity record: %+v", activity.Activity)
	}
}

func TestActivityReportsPhaseProgressAndFailureThroughTheAPIAndEventStream(t *testing.T) {
	t.Parallel()
	s, c := servicetest.Start(t, servicetest.Fixture(t))
	events, _ := fixturekit.EventStream(t, c)
	fixturekit.NextEvent(t, events)

	h := s.StartActivity("activity-test", api.ActivityCheck, fixturekit.Project, fixturekit.Stream, "selecting checks")
	fixturekit.AwaitEvents(t, events, api.Event{Kind: api.EventActivity})
	out, err := c.Activity(context.Background())
	fixturekit.Must(t, err)
	if len(out.Activity) != 1 {
		t.Fatalf("expected one activity entry after start, got %+v", out.Activity)
	}
	e := out.Activity[0]
	if e.Kind != api.ActivityCheck || e.Project != fixturekit.Project || e.Workstream != fixturekit.Stream || e.Phase != "selecting checks" ||
		e.Progress != nil || e.Failure != "" || e.LastProgress.Before(e.StartedAt) {
		t.Fatalf("unexpected entry after start: %+v", e)
	}
	afterStart := e.LastProgress

	h.Progress("running checks", floatPtr(0.5))
	fixturekit.AwaitEvents(t, events, api.Event{Kind: api.EventActivity})
	out, err = c.Activity(context.Background())
	fixturekit.Must(t, err)
	e = out.Activity[0]
	if e.Phase != "running checks" || e.Progress == nil || *e.Progress != 0.5 || e.LastProgress.Before(afterStart) {
		t.Fatalf("progress not reported: %+v", e)
	}

	h.Finish(errors.New("boom"))
	fixturekit.AwaitEvents(t, events, api.Event{Kind: api.EventActivity})
	out, err = c.Activity(context.Background())
	fixturekit.Must(t, err)
	if len(out.Activity) != 1 {
		t.Fatalf("a failed operation should stay visible: %+v", out.Activity)
	}
	e = out.Activity[0]
	if e.Failure != "boom" || e.Phase != "running checks" {
		t.Fatalf("failure not reported: %+v", e)
	}

	fresh := s.StartActivity("activity-test", api.ActivityCheck, fixturekit.Project, fixturekit.Stream, "selecting checks")
	out, err = c.Activity(context.Background())
	fixturekit.Must(t, err)
	if len(out.Activity) != 1 || out.Activity[0].Failure != "" {
		t.Fatalf("a fresh start did not supersede the failed entry: %+v", out.Activity)
	}
	h.Finish(errors.New("stale"))
	out, err = c.Activity(context.Background())
	fixturekit.Must(t, err)
	if len(out.Activity) != 1 || out.Activity[0].Failure != "" {
		t.Fatalf("a superseded handle still reported: %+v", out.Activity)
	}

	fresh.Finish(nil)
	out, err = c.Activity(context.Background())
	fixturekit.Must(t, err)
	if len(out.Activity) != 0 {
		t.Fatalf("a successful finish did not remove the entry: %+v", out.Activity)
	}
}

func TestActivityStaysVisibleAndRespondsWhileAFactoryOperationIsBlocked(t *testing.T) {
	t.Parallel()
	blocking := fixturekit.NewBlockedFactoryOperation()
	_, c, repository := servicetest.BusyReadsService(t, blocking)
	fixturekit.RequestFactoryOperation(t, repository, fixturekit.Stream, "activity-blocked")

	select {
	case <-blocking.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.Release)

	started := time.Now()
	out, err := c.Activity(context.Background())
	fixturekit.Must(t, err)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("GET /activity took %s while a factory operation was blocked", elapsed)
	}
	idx := slices.IndexFunc(out.Activity, func(e api.ActivityEntry) bool { return e.Phase == "fake-factory-op" })
	if idx == -1 {
		t.Fatalf("activity did not show the blocked operation: %+v", out.Activity)
	}
	e := out.Activity[idx]
	if e.Kind != api.ActivityGit || e.Failure != "" || e.LastProgress.IsZero() || e.LastProgress.Before(e.StartedAt) {
		t.Fatalf("unexpected activity entry while blocked: %+v", e)
	}
}
