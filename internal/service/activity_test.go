package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func floatPtr(v float64) *float64 { return &v }

// TestActivityStaysVisibleAndRespondsWhileAFactoryOperationIsBlocked covers
// spec#4: a RepositoryBoundary operation the reconcile controller runs
// inside trace.Repository.WithOperation, which holds the repository's
// operationMu for the whole Apply call (busy_reads_test.go,
// blockedFactoryOperation), stays visible in the activity record with its
// kind and phase while that lock is held, and GET /v1/activity answers
// within one second.
func TestActivityStaysVisibleAndRespondsWhileAFactoryOperationIsBlocked(t *testing.T) {
	t.Parallel()
	blocking := newBlockedFactoryOperation()
	_, c, repository := setUpBusyReadsService(t, blocking)
	requestFactoryOperation(t, repository, stream, "activity-blocked")

	select {
	case <-blocking.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.release)

	started := time.Now()
	out, err := c.Activity(context.Background())
	must(t, err)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("GET /activity took %s while a factory operation was blocked", elapsed)
	}
	idx := slices.IndexFunc(out.Activity, func(e ActivityEntry) bool { return e.Phase == "fake-factory-op" })
	if idx == -1 {
		t.Fatalf("activity did not show the blocked operation: %+v", out.Activity)
	}
	e := out.Activity[idx]
	if e.Kind != ActivityGit || e.Failure != "" || e.LastProgress.IsZero() || e.LastProgress.Before(e.StartedAt) {
		t.Fatalf("unexpected activity entry while blocked: %+v", e)
	}
}

// TestActivityReportsPhaseProgressAndFailureThroughTheAPIAndEventStream
// covers spec#4 and spec#5's "event stream" half: a started operation's
// phase, progress and last-progress time are readable through GET
// /v1/activity as it reports them, each report announces an activity event
// on the stream, a failure keeps the entry visible, and a successful finish
// removes it.
func TestActivityReportsPhaseProgressAndFailureThroughTheAPIAndEventStream(t *testing.T) {
	t.Parallel()
	s, c := start(t, fixture(t))
	events, _ := eventStream(t, c)
	nextEvent(t, events) // the stream's first event is always a resync.

	h := s.StartActivity("activity-test", ActivityCheck, project, stream, "selecting checks")
	awaitEvents(t, events, Event{Kind: EventActivity})
	out, err := c.Activity(context.Background())
	must(t, err)
	if len(out.Activity) != 1 {
		t.Fatalf("expected one activity entry after start, got %+v", out.Activity)
	}
	e := out.Activity[0]
	if e.Kind != ActivityCheck || e.Project != project || e.Workstream != stream || e.Phase != "selecting checks" ||
		e.Progress != nil || e.Failure != "" || e.LastProgress.Before(e.StartedAt) {
		t.Fatalf("unexpected entry after start: %+v", e)
	}
	afterStart := e.LastProgress

	h.Progress("running checks", floatPtr(0.5))
	awaitEvents(t, events, Event{Kind: EventActivity})
	out, err = c.Activity(context.Background())
	must(t, err)
	e = out.Activity[0]
	if e.Phase != "running checks" || e.Progress == nil || *e.Progress != 0.5 || e.LastProgress.Before(afterStart) {
		t.Fatalf("progress not reported: %+v", e)
	}

	h.Finish(errors.New("boom"))
	awaitEvents(t, events, Event{Kind: EventActivity})
	out, err = c.Activity(context.Background())
	must(t, err)
	if len(out.Activity) != 1 {
		t.Fatalf("a failed operation should stay visible: %+v", out.Activity)
	}
	e = out.Activity[0]
	if e.Failure != "boom" || e.Phase != "running checks" {
		t.Fatalf("failure not reported: %+v", e)
	}

	// A fresh start under the same key supersedes the failed entry, and a
	// nil Finish removes it once it succeeds.
	fresh := s.StartActivity("activity-test", ActivityCheck, project, stream, "selecting checks")
	out, err = c.Activity(context.Background())
	must(t, err)
	if len(out.Activity) != 1 || out.Activity[0].Failure != "" {
		t.Fatalf("a fresh start did not supersede the failed entry: %+v", out.Activity)
	}
	// The superseded handle can no longer report for the key.
	h.Finish(errors.New("stale"))
	out, err = c.Activity(context.Background())
	must(t, err)
	if len(out.Activity) != 1 || out.Activity[0].Failure != "" {
		t.Fatalf("a superseded handle still reported: %+v", out.Activity)
	}

	fresh.Finish(nil)
	out, err = c.Activity(context.Background())
	must(t, err)
	if len(out.Activity) != 0 {
		t.Fatalf("a successful finish did not remove the entry: %+v", out.Activity)
	}
}

// TestHealthReportsBackendAvailabilityFromTheActivityRecord covers spec#5:
// health reports the API available and the backend ready with no blocking
// work, not ready while a recovery is in progress, and degraded, naming the
// cause, when an operation has failed or made no progress for
// ActivityStallThreshold.
func TestHealthReportsBackendAvailabilityFromTheActivityRecord(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{At: demoStart}
	opts := fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := start(t, opts)
	ctx := context.Background()

	ready, err := c.Health(ctx)
	must(t, err)
	if !ready.API || !ready.Ready || ready.Backend != BackendReady || ready.Cause != "" {
		t.Fatalf("expected a ready backend with nothing in flight: %+v", ready)
	}
	// Start's own recovery steps report through the same record; by the
	// time it returns, each has finished and left nothing behind.
	activity, err := c.Activity(ctx)
	must(t, err)
	if len(activity.Activity) != 0 {
		t.Fatalf("startup recovery left activity behind: %+v", activity.Activity)
	}

	recovering := s.StartActivity("health-recovery", ActivityRecovery, project, "", "recovering interrupted workspace operations")
	notReady, err := c.Health(ctx)
	must(t, err)
	if !notReady.API || notReady.Ready || notReady.Backend != BackendNotReady || notReady.Cause == "" {
		t.Fatalf("expected a not-ready backend while recovery is in progress: %+v", notReady)
	}
	recovering.Finish(nil)

	failing := s.StartActivity("health-failure", ActivityGit, project, stream, "rebase")
	failing.Finish(errors.New("the upstream branch moved"))
	degradedByFailure, err := c.Health(ctx)
	must(t, err)
	if !degradedByFailure.API || degradedByFailure.Ready || degradedByFailure.Backend != BackendDegraded ||
		!strings.Contains(degradedByFailure.Cause, "the upstream branch moved") {
		t.Fatalf("expected a degraded backend naming the failure: %+v", degradedByFailure)
	}
	failing.Finish(nil)

	// The stall rule only catches an operation once its own reporting has
	// demonstrably started and then stopped (activity.go, reported); a bare
	// start with no progress report is covered by
	// TestHealthDoesNotDegradeOnAGitOperationThatNeverReportsProgress below.
	stalled := s.StartActivity("health-stall", ActivityGit, project, stream, "cloning")
	stalled.Progress("cloning", nil)
	clock.Advance(ActivityStallThreshold)
	degradedByStall, err := c.Health(ctx)
	must(t, err)
	if !degradedByStall.API || degradedByStall.Ready || degradedByStall.Backend != BackendDegraded ||
		!strings.Contains(degradedByStall.Cause, "cloning") {
		t.Fatalf("expected a degraded backend naming the stalled operation: %+v", degradedByStall)
	}
	stalled.Finish(nil)

	readyAgain, err := c.Health(ctx)
	must(t, err)
	if !readyAgain.API || !readyAgain.Ready || readyAgain.Backend != BackendReady || readyAgain.Cause != "" {
		t.Fatalf("expected a ready backend once every operation finished: %+v", readyAgain)
	}
}

// TestHealthDoesNotDegradeOnAGitOperationThatNeverReportsProgress covers
// spec#5: activityReconciler.Apply reports a reconciled Git or workspace
// operation (seal, build, land, rebase, drift, publish) with only a start
// and a finish, nothing in between, so an ordinary one that simply takes
// longer than ActivityStallThreshold must not read degraded. The stall rule
// only applies once an entry's own reporting has demonstrably started and
// then gone quiet (activity.go, ActivityEntry.reported): a bare start with
// no progress report yet says nothing about whether the operation is stuck.
func TestHealthDoesNotDegradeOnAGitOperationThatNeverReportsProgress(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{At: demoStart}
	opts := fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := start(t, opts)
	ctx := context.Background()

	running := s.StartActivity("long-git", ActivityGit, project, stream, "pushing")
	clock.Advance(10 * ActivityStallThreshold)

	stillReady, err := c.Health(ctx)
	must(t, err)
	if !stillReady.API || !stillReady.Ready || stillReady.Backend != BackendReady || stillReady.Cause != "" {
		t.Fatalf("expected a ready backend while a Git operation that never reports progress is still running: %+v", stillReady)
	}

	// Once it does report progress and then goes quiet past the threshold,
	// the same entry is caught.
	running.Progress("pushing", nil)
	clock.Advance(ActivityStallThreshold)
	degraded, err := c.Health(ctx)
	must(t, err)
	if !degraded.API || degraded.Ready || degraded.Backend != BackendDegraded || !strings.Contains(degraded.Cause, "pushing") {
		t.Fatalf("expected a degraded backend once the operation reported progress and then stalled: %+v", degraded)
	}
	running.Finish(nil)
}

// TestHealthDoesNotDegradeOnOrdinaryLongRunningTurnsAndChecks covers spec#5:
// an agent turn or a check run is reported once at start and once at
// finish, with nothing in between, and routinely runs well past
// ActivityStallThreshold under this project's own checks_timeout and
// profile timeout. Health must still report ready while one of those is in
// flight: the no-progress stall rule applies to kinds whose own reporting
// is expected to keep moving, not to a turn or a check that is merely busy.
func TestHealthDoesNotDegradeOnOrdinaryLongRunningTurnsAndChecks(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{At: demoStart}
	opts := fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := start(t, opts)
	ctx := context.Background()

	turn := s.StartActivity("long-turn", ActivityAgentTurn, project, stream, "mason turn")
	check := s.StartActivity("long-check", ActivityCheck, project, stream, "running checks")
	clock.Advance(10 * ActivityStallThreshold)

	stillReady, err := c.Health(ctx)
	must(t, err)
	if !stillReady.API || !stillReady.Ready || stillReady.Backend != BackendReady || stillReady.Cause != "" {
		t.Fatalf("expected a ready backend while a turn and a check are still running: %+v", stillReady)
	}

	turn.Finish(nil)
	check.Finish(nil)
}

// TestHealthStopsDegradingOnAFailureNothingRetries covers spec#5: many
// operations are keyed by the trace event that requested them, not by a
// retry-stable identity (activity.go, ActivityFailureExpiry), so an
// abandoned workstream or a check run superseded by a new run number may
// never issue a fresh start under the same key to supersede a failed entry.
// A failure that nothing retries must still stop degrading health once
// ActivityFailureExpiry passes, and must stop being listed too, while one a
// caller is still retrying under a fresh key is unaffected.
func TestHealthStopsDegradingOnAFailureNothingRetries(t *testing.T) {
	t.Parallel()
	clock := &fixedClock{At: demoStart}
	opts := fixture(t)
	opts.Reconciliation.Now = clock.Now
	s, c := start(t, opts)
	ctx := context.Background()

	abandoned := s.StartActivity("abandoned-check-run-1", ActivityCheck, project, stream, "running checks")
	abandoned.Finish(errors.New("unit workstream was abandoned"))

	degraded, err := c.Health(ctx)
	must(t, err)
	if !degraded.API || degraded.Ready || degraded.Backend != BackendDegraded {
		t.Fatalf("expected a degraded backend right after the unretried failure: %+v", degraded)
	}

	clock.Advance(ActivityFailureExpiry - time.Second)
	stillDegraded, err := c.Health(ctx)
	must(t, err)
	if stillDegraded.Ready || stillDegraded.Backend != BackendDegraded {
		t.Fatalf("expected the failure to still degrade health just before it expires: %+v", stillDegraded)
	}

	clock.Advance(time.Second)
	ready, err := c.Health(ctx)
	must(t, err)
	if !ready.API || !ready.Ready || ready.Backend != BackendReady || ready.Cause != "" {
		t.Fatalf("expected an unretried failure to stop degrading health once it expires: %+v", ready)
	}
	activity, err := c.Activity(ctx)
	must(t, err)
	if len(activity.Activity) != 0 {
		t.Fatalf("expected the expired failure to drop out of the activity record: %+v", activity.Activity)
	}
}
