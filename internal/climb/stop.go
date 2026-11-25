package climb

import (
	"fmt"
	"slices"
	"time"
)

// The stop rules of a climb spec, by name.
const (
	RuleAttempts  = "max_attempts"
	RuleCost      = "max_cost_usd"
	RuleWallClock = "max_wall_clock"
	RulePlateau   = "plateau"
	RuleTarget    = "target"
)

// Firing is a stop rule that holds. A hard rule cannot be continued past
// without an owner budget extension; a soft one may be continued by Jev at its
// bar.
type Firing struct {
	Rule   string `json:"rule"`
	Hard   bool   `json:"hard"`
	Reason string `json:"reason"`
}

// StopInput is everything the stop rules read, after an attempt was recorded.
type StopInput struct {
	Rules     StopRules
	Direction Direction
	// Baseline is the calibrated score the climb started from, and Attempts
	// the recorded attempts in order.
	Baseline float64
	Attempts []Attempt
	// Extension is what the owner's budget extensions add to the hard rules.
	Extension Totals
	// Continued lists the attempts after which a climb.stop exit let the climb
	// go on, and Spent the cost of the climb so far.
	Continued []StopRecord
	Spent     float64
	Started   time.Time
	Now       time.Time
}

// Best returns the primary score of the current best and the number of the
// attempt that made it, which is zero for the baseline.
func Best(baseline float64, attempts []Attempt) (score float64, number int) {
	score = baseline
	for _, a := range attempts {
		if a.Outcome == OutcomeKept && a.Scores != nil {
			score, number = a.Scores.Primary, a.Number
		}
	}
	return score, number
}

// EvaluateStops returns the stop rules that hold after the last recorded
// attempt, the hard ones first. It is plain arithmetic over the records, so a
// restart derives the same answer.
func EvaluateStops(in StopInput) []Firing {
	var out []Firing
	n := len(in.Attempts)
	if n == 0 {
		return nil
	}
	last := in.Attempts[n-1].Number
	if limit := in.Rules.MaxAttempts + in.Extension.Attempts; in.Rules.MaxAttempts > 0 && n >= limit {
		out = append(out, Firing{Rule: RuleAttempts, Hard: true, Reason: fmt.Sprintf("%d attempts were recorded, reaching the limit of %d", n, limit)})
	}
	if in.Rules.MaxCostUSD > 0 {
		if limit := in.Rules.MaxCostUSD + in.Extension.CostUSD; in.Spent >= limit {
			out = append(out, Firing{Rule: RuleCost, Hard: true, Reason: fmt.Sprintf("the climb spent $%.2f, reaching the limit of $%.2f", in.Spent, limit)})
		}
	}
	if in.Rules.MaxWallClock != "" {
		if d, err := time.ParseDuration(in.Rules.MaxWallClock); err == nil && !in.Started.IsZero() {
			if limit := d + in.Extension.WallClock; in.Now.Sub(in.Started) >= limit {
				out = append(out, Firing{Rule: RuleWallClock, Hard: true, Reason: fmt.Sprintf("the climb ran %s, reaching the limit of %s", in.Now.Sub(in.Started).Round(time.Second), limit)})
			}
		}
	}
	best, keptAt := Best(in.Baseline, in.Attempts)
	if in.Rules.Plateau > 0 {
		from := keptAt
		for _, c := range in.Continued {
			if c.Attempt <= last && slices.Contains(c.Rules, RulePlateau) {
				from = max(from, c.Attempt)
			}
		}
		if since := last - from; since >= in.Rules.Plateau {
			out = append(out, Firing{Rule: RulePlateau, Reason: fmt.Sprintf("%d attempts passed since the last improvement, reaching the plateau of %d", since, in.Rules.Plateau)})
		}
	}
	if in.Rules.Target != nil && in.Direction.Valid() && in.Direction.Improvement(best, *in.Rules.Target) >= 0 {
		acknowledged := slices.ContainsFunc(in.Continued, func(c StopRecord) bool { return slices.Contains(c.Rules, RuleTarget) })
		if !acknowledged {
			out = append(out, Firing{Rule: RuleTarget, Reason: fmt.Sprintf("the best score %v reached the target %v", best, *in.Rules.Target)})
		}
	}
	return out
}

// Hard reports whether any firing rule is a hard one.
func Hard(firings []Firing) bool {
	return slices.ContainsFunc(firings, func(f Firing) bool { return f.Hard })
}

// Rules lists the names of the firing rules.
func Rules(firings []Firing) []string {
	out := make([]string, len(firings))
	for i, f := range firings {
		out[i] = f.Rule
	}
	return out
}
