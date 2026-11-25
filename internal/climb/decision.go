package climb

import (
	"fmt"
	"strings"
)

// Cause names why an attempt was discarded.
type Cause string

const (
	// CauseInvalid is a measurement in which a run failed or was invalid.
	CauseInvalid Cause = "invalid-measurement"
	// CauseGuardrails is a guardrail check that did not pass.
	CauseGuardrails Cause = "guardrails-failed"
	// CauseLimits is a secondary metric outside its limit.
	CauseLimits Cause = "limit-violated"
	// CauseNoImprovement is a score that does not beat the best by the minimum
	// improvement.
	CauseNoImprovement Cause = "no-improvement"
	// CauseProtected is a diff that touches a protected path.
	CauseProtected Cause = "protected-path"
	// CauseNoCandidate is an attempt whose mason produced no candidate.
	CauseNoCandidate Cause = "no-candidate"
	// CauseRejected is a kept candidate that its reviewer rejected.
	CauseRejected Cause = "review-rejected"
	// CauseStale is an attempt whose parent is no longer the climb branch's
	// tip, so its candidate could not land on the best.
	CauseStale Cause = "stale-parent"
	// CauseConcluded is an attempt still in flight when the climb was
	// concluded.
	CauseConcluded Cause = "concluded"
)

// Limit bounds a secondary metric. A nil bound is not checked, and a bound a
// value equals is met.
type Limit struct {
	Metric string   `json:"metric"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
}

// Verify returns why value breaks the limit, or "" when it is met.
func (l Limit) Verify(value float64) string {
	switch {
	case l.Max != nil && value > *l.Max:
		return fmt.Sprintf("%s is %v, above its limit %v", l.Metric, value, *l.Max)
	case l.Min != nil && value < *l.Min:
		return fmt.Sprintf("%s is %v, below its limit %v", l.Metric, value, *l.Min)
	}
	return ""
}

// Candidate is everything the keep/discard decision reads.
type Candidate struct {
	Measurement Measurement
	// Best is the primary score of the current best.
	Best      float64
	Direction Direction
	// MinImprovement is the absolute improvement an attempt must reach.
	MinImprovement float64
	// ChecksGreen reports that every guardrail check passed, and ChecksDetail
	// says what failed when it did not.
	ChecksGreen  bool
	ChecksDetail string
	Limits       []Limit
}

// Decision is the keep/discard result. Change is the score minus the best,
// and Improvement is the same distance measured in the metric's direction,
// positive when better. Both are zero when the measurement is invalid.
type Decision struct {
	Keep        bool    `json:"keep"`
	Cause       Cause   `json:"cause,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Change      float64 `json:"change"`
	Improvement float64 `json:"improvement"`
}

// Decide keeps a candidate only when its measurement is valid, it beats the
// best by at least the minimum improvement in the metric's direction, its
// guardrail checks are green and its secondary limits are met. An improvement
// equal to the minimum counts. The cause of a discard is the first condition
// that fails, in that order, and the reason lists every failing condition of
// a valid measurement.
func Decide(c Candidate) Decision {
	if !c.Measurement.Valid {
		return Decision{Cause: CauseInvalid, Reason: "the measurement is invalid: " + c.Measurement.Reason}
	}
	if !c.Direction.Valid() || !(c.MinImprovement > 0) {
		return Decision{Cause: CauseInvalid, Reason: fmt.Sprintf("direction %q and minimum improvement %v do not describe a metric", c.Direction, c.MinImprovement)}
	}
	d := Decision{Change: c.Measurement.Score - c.Best, Improvement: c.Direction.Improvement(c.Measurement.Score, c.Best)}
	var cause Cause
	var reasons []string
	fail := func(why Cause, format string, args ...any) {
		if cause == "" {
			cause = why
		}
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}
	if !c.ChecksGreen {
		detail := c.ChecksDetail
		if detail == "" {
			detail = "a guardrail check did not pass"
		}
		fail(CauseGuardrails, "%s", detail)
	}
	for _, l := range c.Limits {
		value, ok := c.Measurement.Metrics[l.Metric]
		if !ok {
			fail(CauseLimits, "%s has no measured value to hold to its limit", l.Metric)
			continue
		}
		if why := l.Verify(value); why != "" {
			fail(CauseLimits, "%s", why)
		}
	}
	if !atLeast(d.Improvement, c.MinImprovement) {
		fail(CauseNoImprovement, "the score %v improves on the best %v by %v, short of the minimum improvement %v", c.Measurement.Score, c.Best, d.Improvement, c.MinImprovement)
	}
	d.Keep, d.Cause, d.Reason = cause == "", cause, strings.Join(reasons, "; ")
	return d
}
