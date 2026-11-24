package scheduler

import (
	"time"
)

// Explanation names what legitimately explains a unit or workstream that has
// not progressed: something the factory is waiting on, or doing, so the quiet
// is not a stall.
type Explanation string

const (
	// NotExplained: nothing explains the quiet, so the subject is stalled.
	NotExplained Explanation = ""
	// ExplainedByCard: an owner card is open for the subject.
	ExplainedByCard Explanation = "owner_card"
	// ExplainedByPause: a pause covers the subject.
	ExplainedByPause Explanation = "pause"
	// ExplainedByTurn: a turn of the subject is running or queued, and younger
	// than the window.
	ExplainedByTurn Explanation = "turn"
	// ExplainedByOperation: an operation of the subject is in flight and still
	// can reach a result.
	ExplainedByOperation Explanation = "operation"
	// ExplainedByDependency: the subject waits on an upstream that is alive.
	ExplainedByDependency Explanation = "dependency_wait"
)

// HoldCause is why a hold keeps an operation from running. A hold explains a
// quiet only when its own cause does: a pause, an open owner card or a live
// dependency wait. Any other cause (a missing credential, a role switched off,
// a workgroup joining) leaves the operation held with nothing behind it.
type HoldCause string

const (
	// NotHeld: nothing holds the operation.
	NotHeld HoldCause = ""
	// HeldByPause: a pause covers the operation's workstream.
	HeldByPause HoldCause = "pause"
	// HeldByCard: an open owner card holds the operation.
	HeldByCard HoldCause = "owner_card"
	// HeldByDependency: a live dependency wait holds the operation.
	HeldByDependency HoldCause = "dependency_wait"
	// HeldByOther: any other hold.
	HeldByOther HoldCause = "other"
)

func (c HoldCause) explanation() (Explanation, bool) {
	switch c {
	case HeldByPause:
		return ExplainedByPause, true
	case HeldByCard:
		return ExplainedByCard, true
	case HeldByDependency:
		return ExplainedByDependency, true
	}
	return NotExplained, false
}

// Operation is an operation of a subject that has no result yet, as the stall
// watchdog reads it.
type Operation struct {
	// Action names the operation (a landing, a rebase, a publication, a check
	// run).
	Action string
	// LastAttempt is the time of the operation's latest claim, observation or
	// retry; zero when it has not been attempted.
	LastAttempt time.Time
	// Failures is the number of consecutive failed attempts.
	Failures int
	// LastFailure is why the latest attempt failed, which names a recovery
	// failure of the workspace when that was the cause.
	LastFailure string
	// Held is the cause of the hold keeping the operation from running, if one
	// does.
	Held HoldCause
}

// Explains reports whether the operation explains a quiet at now, and the
// explanation. An operation is in flight, and explains it, only while its
// latest claim, observation or retry is younger than after and its consecutive
// failures are below the give-up limit, so an operation that can no longer reach
// a result explains nothing. A held operation explains it only when its hold's
// own cause does.
func (o Operation) Explains(now time.Time, after time.Duration, limit int) (Explanation, bool) {
	if o.Held != NotHeld {
		return o.Held.explanation()
	}
	if o.Failures >= limit || o.LastAttempt.IsZero() || now.Sub(o.LastAttempt) >= after {
		return NotExplained, false
	}
	return ExplainedByOperation, true
}

// Quiet is what the stall watchdog read about one unit or workstream that has
// made no progress since Since.
type Quiet struct {
	// Since is when the subject last progressed. A turn of the chief of staff
	// is not progress.
	Since time.Time
	// Card says an owner card is open for the subject.
	Card bool
	// Paused says a pause covers the subject.
	Paused bool
	// Turn is the latest time a turn of the subject, other than the chief of
	// staff's, that is running or queued was queued, claimed or attempted; zero
	// when none is. A turn explains the quiet only while that time is younger
	// than the window, so a turn stuck in its queue is a stall.
	Turn time.Time
	// Waiting says the subject waits on an upstream that is itself progressing
	// or explained.
	Waiting bool
	// Operations are the operations of the subject that have no result.
	Operations []Operation
}

// Stall is the verdict on a quiet subject.
type Stall struct {
	// Stalled says the subject has not progressed for the window and nothing
	// explains it.
	Stalled bool
	// Quiet is how long the subject has been quiet.
	Quiet time.Duration
	// Explanation is what explains the quiet, empty when Stalled.
	Explanation Explanation
	// Unresolved are the operations of a stalled subject: none of them explains
	// the quiet, so the last failure each carries is the evidence a stalled exit
	// cites.
	Unresolved []Operation
}

// Explain returns what explains a subject's quiet at now, or NotExplained. It
// does not depend on how long the subject has been quiet. limit is the number of
// consecutive failures that ends an operation.
func Explain(q Quiet, now time.Time, after time.Duration, limit int) Explanation {
	switch {
	case q.Card:
		return ExplainedByCard
	case q.Paused:
		return ExplainedByPause
	case !q.Turn.IsZero() && now.Sub(q.Turn) < after:
		return ExplainedByTurn
	case q.Waiting:
		return ExplainedByDependency
	}
	for _, o := range q.Operations {
		if why, ok := o.Explains(now, after, limit); ok {
			return why
		}
	}
	return NotExplained
}

// Assess judges a subject quiet since q.Since at now: stalled when it has not
// progressed for after and none of the listed explanations holds. limit is the
// number of consecutive failures that ends an operation. A window of zero or
// less turns the watchdog off: nothing is stalled.
func Assess(q Quiet, now time.Time, after time.Duration, limit int) Stall {
	out := Stall{Quiet: now.Sub(q.Since)}
	if after <= 0 || q.Since.IsZero() || out.Quiet < after {
		return out
	}
	if out.Explanation = Explain(q, now, after, limit); out.Explanation != NotExplained {
		return out
	}
	out.Stalled = true
	out.Unresolved = append(out.Unresolved, q.Operations...)
	return out
}
