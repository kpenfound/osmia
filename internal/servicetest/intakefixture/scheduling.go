package intakefixture

import (
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// PollAttempt is the test-only snapshot needed by the fixture clock.
type PollAttempt struct {
	Project config.ProjectID
	At      time.Time
}

// NextPollDue returns the project's earliest polling deadline after now.
func NextPollDue(attempts []PollAttempt, project config.ProjectID, interval time.Duration, now time.Time) time.Time {
	var next time.Time
	for _, attempt := range attempts {
		if attempt.Project != project {
			continue
		}
		due := attempt.At.Add(interval)
		if due.After(now) && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	return next
}

// ScheduleCounter counts completed reconciliation passes through a test hook.
func ScheduleCounter() (*atomic.Int64, func()) {
	passed := &atomic.Int64{}
	return passed, func() { passed.Add(1) }
}
