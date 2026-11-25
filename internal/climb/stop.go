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

// Limits are the attempt, cost and wall-clock limits the stop rules enforce:
// the climb spec's hard rules plus what the owner's budget extensions add. A
// zero limit means the climb spec sets none, and an extension does not create
// one.
type Limits struct {
	Attempts  int
	CostUSD   float64
	WallClock time.Duration
}

// Limits returns the hard limits with the owner's budget extensions applied.
// The stop rules and the climb panel's budget meters both read them from here,
// so what the panel shows is what the climb loop enforces.
func (r StopRules) Limits(ext Totals) Limits {
	var l Limits
	if r.MaxAttempts > 0 {
		l.Attempts = r.MaxAttempts + ext.Attempts
	}
	if r.MaxCostUSD > 0 {
		l.CostUSD = r.MaxCostUSD + ext.CostUSD
	}
	if r.MaxWallClock != "" {
		if d, err := time.ParseDuration(r.MaxWallClock); err == nil && d > 0 {
			l.WallClock = d + ext.WallClock
		}
	}
	return l
}

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
	// Baseline is the calibrated score of the current epoch, and Attempts the
	// recorded attempts in order. The budgets count every attempt, since each
	// one cost what it cost; the best, the plateau and the target read only
	// the attempts of Epoch, the ones measured against Baseline.
	Baseline float64
	Attempts []Attempt
	Epoch    int
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
	limits := in.Rules.Limits(in.Extension)
	if limits.Attempts > 0 && n >= limits.Attempts {
		out = append(out, Firing{Rule: RuleAttempts, Hard: true, Reason: fmt.Sprintf("%d attempts were recorded, reaching the limit of %d", n, limits.Attempts)})
	}
	if limits.CostUSD > 0 && in.Spent >= limits.CostUSD {
		out = append(out, Firing{Rule: RuleCost, Hard: true, Reason: fmt.Sprintf("the climb spent $%.2f, reaching the limit of $%.2f", in.Spent, limits.CostUSD)})
	}
	if limits.WallClock > 0 && !in.Started.IsZero() && in.Now.Sub(in.Started) >= limits.WallClock {
		out = append(out, Firing{Rule: RuleWallClock, Hard: true, Reason: fmt.Sprintf("the climb ran %s, reaching the limit of %s", in.Now.Sub(in.Started).Round(time.Second), limits.WallClock)})
	}
	best, keptAt := Best(in.Baseline, InEpoch(in.Attempts, in.Epoch))
	if in.Rules.Plateau > 0 {
		from := max(keptAt, lastBefore(in.Attempts, in.Epoch))
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

// lastBefore returns the number of the last attempt that ran before the epoch
// began, which is zero when none did.
func lastBefore(attempts []Attempt, epoch int) int {
	last := 0
	for _, a := range attempts {
		if a.EpochNumber() < max(epoch, 1) {
			last = max(last, a.Number)
		}
	}
	return last
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
