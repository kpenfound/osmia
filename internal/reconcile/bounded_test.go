package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// boundedSystem is a fakeSystem whose operation is bounded: it can hang, fail
// the way the workspace layer fails, and end an operation it is asked to give
// up.
type boundedSystem struct {
	*fakeSystem
	mu           sync.Mutex
	mode         string
	hangInspect  bool
	failings     []int
	reasons      []string
	giveUps      []GiveUpReason
	giveUpResult coreadapter.OperationResult
	giveUpErr    error
}

func (b *boundedSystem) setMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mode = mode
}

func (b *boundedSystem) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	b.mu.Lock()
	hang := b.hangInspect
	b.mu.Unlock()
	if hang {
		<-ctx.Done()
		return coreadapter.Observation{}, ctx.Err()
	}
	return b.fakeSystem.Inspect(ctx, op)
}

func (b *boundedSystem) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	b.mu.Lock()
	mode := b.mode
	b.mu.Unlock()
	switch mode {
	case "hang":
		<-ctx.Done()
		return coreadapter.OperationResult{}, ctx.Err()
	case "contended":
		return coreadapter.OperationResult{}, &workspace.ContentionError{Clone: "/clone", Waited: time.Second, Err: context.DeadlineExceeded}
	case "recovery":
		return coreadapter.OperationResult{}, &workspace.RecoveryError{Clone: "/clone", Err: errors.New("the restore hung")}
	case "recovery-after-deadline":
		<-ctx.Done()
		return coreadapter.OperationResult{}, &workspace.RecoveryError{Clone: "/clone", Err: errors.New("the restore hung")}
	case "fail":
		return coreadapter.OperationResult{}, errInjected
	}
	return b.fakeSystem.Apply(ctx, op)
}

func (b *boundedSystem) Bound(coreadapter.Operation) bool { return true }

func (b *boundedSystem) Failing(_ context.Context, _ coreadapter.Operation, failures int, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failings = append(b.failings, failures)
	b.reasons = append(b.reasons, reason)
	return nil
}

func (b *boundedSystem) GiveUp(_ context.Context, _ coreadapter.Operation, why GiveUpReason) (coreadapter.OperationResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.giveUps = append(b.giveUps, why)
	return b.giveUpResult, b.giveUpErr
}

func (b *boundedSystem) given() []GiveUpReason {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]GiveUpReason(nil), b.giveUps...)
}

// permits counts the capacity an attempt takes and gives back.
type permits struct {
	mu       sync.Mutex
	taken    int
	released int
}

func (p *permits) limit(coreadapter.Operation) (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.taken++
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.released++
	}, true
}

func (p *permits) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.taken, p.released
}

type boundedFixture struct {
	*fixture
	bounded *boundedSystem
	permits *permits
}

func newBoundedFixture(t *testing.T) *boundedFixture {
	t.Helper()
	f := setup(t, coreadapter.RepositoryBoundary)
	return &boundedFixture{f, &boundedSystem{fakeSystem: f.system, giveUpResult: coreadapter.OperationResult{Outcome: "failed", Evidence: "the operation was refused"}}, &permits{}}
}

func (b *boundedFixture) controller(t *testing.T, options Options) *Controller {
	t.Helper()
	options.Worker, options.Now, options.Limit = "test-controller", b.clock.Now, b.permits.limit
	options.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: b.bounded}
	if options.RetryDelay == 0 {
		options.RetryDelay, options.MaxRetryDelay = time.Minute, time.Minute
	}
	c, err := New(b.repository, options)
	must(t, err)
	return c
}

// lastRetry returns the latest failure the operation recorded.
func (b *boundedFixture) lastRetry(t *testing.T) trace.OperationAction {
	t.Helper()
	record := b.record(t)
	for i := len(record.History) - 1; i >= 0; i-- {
		if record.History[i].Kind == RetryAction {
			return record.History[i]
		}
	}
	t.Fatalf("the operation recorded no retry: %+v", record.History)
	return trace.OperationAction{}
}

// addOperation publishes a second operation of the workstream.
func (b *boundedFixture) addOperation(t *testing.T, name string) {
	t.Helper()
	event := trace.Event{ID: trace.EventID(name, "effect"), Kind: "local-effect", Body: "Perform more local work"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(projectID, streamID, event.ID), Boundary: coreadapter.RepositoryBoundary, Action: "prepare", Input: []byte(`{"scope":{"Project":"test"}}`)}
	tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: name, Revision: 1, Project: projectID, Workstream: streamID, At: b.clock.Now(), Actor: trace.Actor{Kind: "service", ID: "scheduler"}, Cause: "owner-request", Depth: 2}, Subject: name, To: "pending", Reason: "More authorized work"}, Events: []trace.Event{event}}
	_, err := b.repository.Transact(context.Background(), tx)
	must(t, err)
}

// laneIsFree reports whether another attempt of the operation can claim its
// workstream: the attempt that failed released it.
func (b *boundedFixture) laneIsFree(t *testing.T) bool {
	t.Helper()
	b.clock.Advance(time.Hour)
	attempt, _, err := b.repository.BeginOperation(context.Background(), streamID, b.event.ID, trace.WorkflowOperation, trace.Actor{Kind: "service", ID: "test-controller"}, b.clock.Now)
	must(t, err)
	if attempt == nil {
		return false
	}
	attempt.Close()
	return true
}

// An attempt whose effect hangs is cut off at the attempt deadline and recorded
// as a retry that names the deadline. Its capacity and workstream are free
// again, and a later attempt runs.
func TestAttemptIsCutOffAtItsDeadline(t *testing.T) {
	ctx := context.Background()
	b := newBoundedFixture(t)
	b.bounded.setMode("hang")
	c := b.controller(t, Options{AttemptDeadline: 20 * time.Millisecond})
	started := time.Now()
	must(t, c.Pass(ctx))
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the pass took %s", elapsed)
	}
	retry := b.lastRetry(t)
	if !strings.Contains(retry.Failure, "Attempt deadline of 20ms exceeded") {
		t.Fatalf("the retry does not name the deadline: %q", retry.Failure)
	}
	if taken, released := b.permits.counts(); taken != 1 || released != 1 {
		t.Fatalf("capacity taken %d, released %d", taken, released)
	}
	if told := b.bounded.failings; len(told) != 1 || told[0] != 1 || !strings.Contains(b.bounded.reasons[0], "Attempt deadline") {
		t.Fatalf("the adapter was told of failures %v: %q", told, b.bounded.reasons)
	}
	if !b.laneIsFree(t) {
		t.Fatal("the attempt kept its workstream")
	}
	b.bounded.setMode("")
	must(t, c.Pass(ctx))
	b.completed(t)
}

// A hung inspection is cut off the same way.
func TestInspectionIsCutOffAtItsDeadline(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.hangInspect = true
	c := b.controller(t, Options{AttemptDeadline: 20 * time.Millisecond})
	must(t, c.Pass(context.Background()))
	record := b.record(t)
	if got := b.lastRetry(t).Failure; !strings.Contains(got, "Inspection failed") || !strings.Contains(got, "Attempt deadline of 20ms exceeded") {
		t.Fatalf("the retry does not name the deadline: %q", got)
	}
	if record.Result != nil || record.Observation == nil || record.Observation.State != coreadapter.EffectUnknown {
		t.Fatalf("a hung inspection left %+v", record)
	}
	if _, applies := b.system.counts(); applies != 0 {
		t.Fatal("an operation that could not be inspected was applied")
	}
}

// A contention or recovery failure of the workspace layer is recorded as a
// retry that names it, and releases the attempt's capacity and workstream.
func TestWorkspaceFailuresAreRecordedWithTheirOwnReasons(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"contended", "Clone lock contention"},
		{"recovery", "Clone recovery failure"},
		{"recovery-after-deadline", "Clone recovery failure after the attempt deadline of 20ms"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			b := newBoundedFixture(t)
			b.bounded.setMode(tc.mode)
			c := b.controller(t, Options{AttemptDeadline: 20 * time.Millisecond})
			must(t, c.Pass(context.Background()))
			if got := b.lastRetry(t).Failure; !strings.Contains(got, tc.want) {
				t.Fatalf("the retry says %q, want %q", got, tc.want)
			}
			if taken, released := b.permits.counts(); taken != 1 || released != 1 {
				t.Fatalf("capacity taken %d, released %d", taken, released)
			}
			if !b.laneIsFree(t) {
				t.Fatal("the attempt kept its workstream")
			}
		})
	}
}

// failAttempts makes the given number of passes, one a minute apart.
func (b *boundedFixture) failAttempts(t *testing.T, c *Controller, attempts int) {
	t.Helper()
	for range attempts {
		must(t, c.Pass(context.Background()))
		b.clock.Advance(time.Minute)
	}
}

// An operation that fails past the limit, with no durable outcome, is ended by
// the adapter's give-up once the window has also passed, and not before. The
// result is terminal: the operation is acknowledged and releases what it held.
func TestAFailingOperationIsGivenUpAfterTheLimitAndTheWindow(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.setMode("fail")
	c := b.controller(t, Options{GiveUpWindow: 30 * time.Minute})
	// Past the failure count but inside the window: it keeps retrying.
	b.failAttempts(t, c, 2*GiveUpFailures)
	if given := b.bounded.given(); len(given) != 0 {
		t.Fatalf("given up inside the window: %+v", given)
	}
	if record := b.record(t); record.Result != nil || ConsecutiveFailures(record) != 2*GiveUpFailures {
		t.Fatalf("after %d failures: %+v", 2*GiveUpFailures, record)
	}
	// The window passes: the next attempt ends the operation without applying.
	_, appliedBefore := b.system.counts()
	b.clock.Advance(30 * time.Minute)
	must(t, c.Pass(context.Background()))
	given := b.bounded.given()
	if len(given) != 1 {
		t.Fatalf("given up %d times", len(given))
	}
	if given[0].Status.Failures != 2*GiveUpFailures || given[0].Limit.Failures != GiveUpFailures || given[0].Limit.Window != 30*time.Minute || !strings.Contains(given[0].Status.LastFailure, "interrupted") {
		t.Fatalf("given up for %+v", given[0])
	}
	record := b.record(t)
	if !record.Acknowledged || record.Result == nil || record.Result.Outcome != "failed" || record.Result.Evidence != "the operation was refused" {
		t.Fatalf("the operation ended as %+v", record)
	}
	if _, applied := b.system.counts(); applied != appliedBefore {
		t.Fatal("the effect was attempted after the operation was given up")
	}
	// The failure notices came once per failure, in order.
	for i, failures := range b.bounded.failings {
		if failures != i+1 {
			t.Fatalf("the adapter was told of failures %v", b.bounded.failings)
		}
	}
	if taken, released := b.permits.counts(); taken != released {
		t.Fatalf("capacity taken %d, released %d", taken, released)
	}
	// Later operations of the workstream proceed.
	b.bounded.setMode("")
	b.addOperation(t, "later")
	must(t, c.Pass(context.Background()))
	if _, applied := b.system.counts(); applied != appliedBefore+1 {
		t.Fatalf("the workstream's next operation was applied %d times", applied-appliedBefore)
	}
}

// A streak below the failure limit keeps retrying however long ago it began.
func TestAStreakBelowTheLimitKeepsRetrying(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.setMode("fail")
	c := b.controller(t, Options{GiveUpWindow: time.Minute})
	b.failAttempts(t, c, GiveUpFailures-1)
	b.clock.Advance(24 * time.Hour)
	must(t, c.Pass(context.Background()))
	if given := b.bounded.given(); len(given) != 0 {
		t.Fatalf("given up after %d failures: %+v", GiveUpFailures-1, given)
	}
	if record := b.record(t); ConsecutiveFailures(record) != GiveUpFailures || record.Result != nil {
		t.Fatalf("%+v", record)
	}
}

// A success ends the streak: the operation's result is the effect's.
func TestASuccessEndsTheStreak(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.setMode("fail")
	c := b.controller(t, Options{GiveUpWindow: time.Minute})
	b.failAttempts(t, c, GiveUpFailures-1)
	b.bounded.setMode("")
	must(t, c.Pass(context.Background()))
	b.completed(t)
	if given := b.bounded.given(); len(given) != 0 {
		t.Fatalf("given up: %+v", given)
	}
}

// An outcome the adapter recorded is never overwritten: Inspect finding one
// completes the operation with it, even past the limit and the window, and the
// adapter is not asked to give up.
func TestADurableOutcomeWinsOverGivingUp(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.setMode("fail")
	c := b.controller(t, Options{GiveUpWindow: 30 * time.Minute})
	b.failAttempts(t, c, 2*GiveUpFailures)
	b.clock.Advance(30 * time.Minute)
	// The effect completed and recorded its outcome durably, then the service
	// restarted before the controller wrote its result.
	b.system.mu.Lock()
	b.system.effects[b.event.Operation.ID] = coreadapter.OperationResult{Outcome: "succeeded", Evidence: "recorded before the restart"}
	b.system.mu.Unlock()
	b.reopen(t)
	c = b.controller(t, Options{GiveUpWindow: 30 * time.Minute})
	must(t, c.Pass(context.Background()))
	record := b.record(t)
	if record.Result == nil || record.Result.Outcome != "succeeded" || record.Result.Evidence != "recorded before the restart" {
		t.Fatalf("the durable outcome was not kept: %+v", record.Result)
	}
	if given := b.bounded.given(); len(given) != 0 {
		t.Fatalf("given up despite a durable outcome: %+v", given)
	}
}

// A give-up that fails leaves the operation pending for another attempt.
func TestAGiveUpThatFailsIsRetried(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.setMode("fail")
	b.bounded.giveUpErr = errors.New("the remote could not be read")
	c := b.controller(t, Options{GiveUpWindow: 30 * time.Minute})
	b.failAttempts(t, c, 2*GiveUpFailures)
	b.clock.Advance(time.Hour)
	must(t, c.Pass(context.Background()))
	if got := b.lastRetry(t).Failure; !strings.Contains(got, "Give-up reconciliation failed: the remote could not be read") {
		t.Fatalf("the retry says %q", got)
	}
	if record := b.record(t); record.Result != nil || record.Acknowledged {
		t.Fatalf("%+v", record)
	}
	b.bounded.giveUpErr = nil
	b.clock.Advance(time.Hour)
	must(t, c.Pass(context.Background()))
	if record := b.record(t); record.Result == nil || !record.Acknowledged {
		t.Fatalf("%+v", record)
	}
}

// The give-up limit and an operation's attempts are readable by other code.
func TestOperationStatusAndGiveUpLimitAreReadable(t *testing.T) {
	b := newBoundedFixture(t)
	b.bounded.setMode("fail")
	c := b.controller(t, Options{})
	b.failAttempts(t, c, 3)
	record := b.record(t)
	status := StatusOf(record)
	first := record.History[0].At
	if status.Failures != 3 || status.LastFailure != "Effect returned error: interrupted" || status.FirstFailure.Before(first) || !status.LastAttempt.After(status.FirstFailure) {
		t.Fatalf("%+v", status)
	}
	if limit := c.GiveUpLimit(); limit.Failures != GiveUpFailures || limit.Window != DefaultGiveUpWindow {
		t.Fatalf("%+v", limit)
	}
	if limit := NewGiveUpLimit(10 * time.Minute); limit.Window != 10*time.Minute || limit.Reached(status, status.FirstFailure.Add(time.Hour)) {
		t.Fatalf("%+v", limit)
	}
	if !(GiveUpLimit{Failures: 3, Window: time.Minute}).Reached(status, status.FirstFailure.Add(time.Minute)) {
		t.Fatal("a streak at the limit and the window is not reached")
	}
}
