package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// DefaultAttemptDeadline bounds one Inspect or one Apply of a bounded
// operation. A call that is still running when it passes is cancelled and the
// attempt is recorded as a retry.
const DefaultAttemptDeadline = 15 * time.Minute

// DefaultReconcileDeadline bounds the read-only reconciliation a bounded
// operation runs before it gives up.
const DefaultReconcileDeadline = 5 * time.Minute

// GiveUpFailures is the number of consecutive failed attempts after which a
// bounded operation is ended once the give-up window has also passed.
const GiveUpFailures = 5

// DefaultGiveUpWindow is the time since the first failure of a streak that
// must have passed, besides GiveUpFailures failures, before a bounded
// operation is ended. It is the default of the `[loop] stall_after` setting,
// which the service connects to Options.GiveUpWindow.
const DefaultGiveUpWindow = 45 * time.Minute

// GiveUpLimit is the failure streak that ends a bounded operation: at least
// Failures consecutive failures, the first of them Window or longer ago.
type GiveUpLimit struct {
	Failures int
	Window   time.Duration
}

// NewGiveUpLimit returns the limit with the given window, or the default
// window when it is not positive.
func NewGiveUpLimit(window time.Duration) GiveUpLimit {
	if window <= 0 {
		window = DefaultGiveUpWindow
	}
	return GiveUpLimit{Failures: GiveUpFailures, Window: window}
}

// Reached reports whether the streak the status describes has reached the
// limit at now.
func (l GiveUpLimit) Reached(s OperationStatus, now time.Time) bool {
	return s.Failures >= l.Failures && !s.FirstFailure.IsZero() && now.Sub(s.FirstFailure) >= l.Window
}

// OperationStatus is what an operation's history says of its attempts, for
// other code to read without deriving it from logs.
type OperationStatus struct {
	// Failures counts the failed attempts of the operation, all of them
	// consecutive: a result ends its attempts.
	Failures int
	// FirstFailure is when the first failed attempt was recorded; zero when
	// the operation has not failed.
	FirstFailure time.Time
	// LastAttempt is the time of the operation's latest recorded action: its
	// claim, observation, effect, result or retry.
	LastAttempt time.Time
	// LastFailure is the reason the latest failed attempt recorded, which
	// names a recovery failure when that was the cause.
	LastFailure string
}

// StatusOf reads the attempts an operation's record holds.
func StatusOf(record trace.OperationRecord) OperationStatus {
	var s OperationStatus
	for _, action := range record.History {
		if action.At.After(s.LastAttempt) {
			s.LastAttempt = action.At
		}
		if action.Kind != RetryAction {
			continue
		}
		s.Failures++
		if s.FirstFailure.IsZero() {
			s.FirstFailure = action.At
		}
		s.LastFailure = action.Failure
	}
	return s
}

// Bounded is the optional capability of a Reconciler whose operations always
// reach a result. The controller runs each Inspect and Apply of an operation
// the adapter bounds under an attempt deadline, tells the adapter about its
// failures, and, once the operation has failed past the give-up limit without
// a durable outcome, asks the adapter to end it.
type Bounded interface {
	// Bound reports whether the operation is bounded.
	Bound(coreadapter.Operation) bool
	// Failing is told of each failed attempt of a bounded operation before the
	// retry is recorded, with the number of consecutive failures including
	// this one and its reason. It must be idempotent: a crash can repeat it.
	Failing(ctx context.Context, op coreadapter.Operation, failures int, reason string) error
	// GiveUp ends an operation that has failed past the limit and whose Inspect
	// found no durable outcome. It reads, without mutating anything, what the
	// interrupted attempts left, under a deadline of its own, and returns the
	// terminal result: the operation's own success path when its effect is
	// found complete, its refusal when the effect is confirmed absent, and a
	// refusal that says the outcome could not be determined when it cannot be
	// read. An error leaves the operation pending for another attempt.
	GiveUp(ctx context.Context, op coreadapter.Operation, why GiveUpReason) (coreadapter.OperationResult, error)
}

// GiveUpReason is what led a bounded operation to be ended.
type GiveUpReason struct {
	Status OperationStatus
	Limit  GiveUpLimit
}

// Summary is a sentence describing the streak, for a result's evidence.
func (g GiveUpReason) Summary() string {
	return fmt.Sprintf("%d consecutive failures since %s; last failure: %s", g.Status.Failures, g.Status.FirstFailure.UTC().Format(time.RFC3339), g.Status.LastFailure)
}

// boundedAdapter returns the adapter's Bounded capability when it bounds op.
func boundedAdapter(adapter coreadapter.Reconciler, op coreadapter.Operation) (Bounded, bool) {
	b, ok := adapter.(Bounded)
	return b, ok && b.Bound(op)
}

// GiveUpLimit returns the limit that ends this controller's bounded
// operations.
func (c *Controller) GiveUpLimit() GiveUpLimit { return NewGiveUpLimit(c.options.GiveUpWindow) }

func (c *Controller) attemptDeadline() time.Duration {
	if c.options.AttemptDeadline > 0 {
		return c.options.AttemptDeadline
	}
	return DefaultAttemptDeadline
}

func (c *Controller) reconcileDeadline() time.Duration {
	if c.options.ReconcileDeadline > 0 {
		return c.options.ReconcileDeadline
	}
	return DefaultReconcileDeadline
}

// attemptFailure names why an Inspect or Apply that returned err failed. A
// contention or recovery failure of the workspace layer, and an attempt that
// ran into its deadline while the parent context was still live, each have
// their own words, so the retry says which. attempt is the context the call ran
// under and parent the context of the controller.
func attemptFailure(parent, attempt context.Context, deadline time.Duration, err error) string {
	timedOut := parent.Err() == nil && errors.Is(attempt.Err(), context.DeadlineExceeded)
	switch {
	case workspace.IsContention(err):
		return fmt.Sprintf("Clone lock contention: %v", err)
	case workspace.IsRecoveryFailure(err) && timedOut:
		return fmt.Sprintf("Clone recovery failure after the attempt deadline of %s: %v", deadline, err)
	case workspace.IsRecoveryFailure(err):
		return fmt.Sprintf("Clone recovery failure: %v", err)
	case timedOut:
		return fmt.Sprintf("Attempt deadline of %s exceeded: %v", deadline, err)
	}
	return ""
}
