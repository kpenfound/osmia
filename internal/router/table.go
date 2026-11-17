package router

import (
	"fmt"
	"math"

	"github.com/kpenfound/osmia/internal/jev"
)

// TableVersion names the code table that turns Jev's answers into the next
// state. Bump it whenever a row, a bar or a floor changes; a transition
// records the version and the row that chose it.
const TableVersion = "v1"

// The probabilities of table v1. The strict bar is the configured
// [jev] decision_threshold and is given to Decide.
const (
	// ModestBar is the bar of a retry or a send-back.
	ModestBar = 0.7
	// FitsFloor is the probability under which the default is judged not to
	// fit.
	FitsFloor = 0.5
	// LowCeiling is the probability at or under which a proposition is judged
	// unlikely.
	LowCeiling = 0.3
)

// The reasons a router holds a subject without moving it.
const (
	// HoldOrdersDisagree: the two pick questions chose different targets.
	HoldOrdersDisagree = "orders_disagree"
	// HoldBelowBar: the best target is below its bar while the default is
	// judged not to fit, or the trigger's answers do not corroborate it.
	HoldBelowBar = "below_bar"
	// HoldDefaultIllegal: the default fails a hard prerequisite, so there is
	// nothing to keep.
	HoldDefaultIllegal = "default_illegal"
	// HoldRefused: the target could not be applied; see the refusal.
	HoldRefused = "refused"
	// HoldIntent: acting would change approved intent, so the move is the
	// owner's.
	HoldIntent = "intent"
	// HoldStale: the subject moved on since the exit was raised.
	HoldStale = "stale"
)

// Verdict is the table's result for one set of answers.
type Verdict struct {
	Action string
	// Target is the state to enter (ActionCommit) or the move to send to the
	// owner (ActionOwner).
	Target string
	// Rule is the row of the table that decided, as "<n>:<name>".
	Rule string
	// Hold says why the subject is held (ActionHold) or the move goes to the
	// owner.
	Hold string
	// Departed says the verdict leaves the default.
	Departed bool
	// P is the probability of the chosen target: the lower of the two picks'.
	P float64
	// Fits is whether the default was judged to fit.
	Fits bool
}

// level returns the probability a level names, given the strict bar.
func level(l Level, strict float64) float64 {
	switch l {
	case LevelStrict:
		return strict
	case LevelHalf:
		return FitsFloor
	case LevelLow:
		return LowCeiling
	}
	return ModestBar
}

func barOf(t Target, strict float64) float64 {
	if t.Bar == BarStrict {
		return strict
	}
	return ModestBar
}

// corroborated reports whether the trigger's own answers show what the target
// needs for a departure.
func corroborated(t Target, own map[string]float64, strict float64) bool {
	for _, c := range t.Corroborate {
		p, ok := own[c.Question]
		switch {
		case !ok:
			return false
		case c.AtLeast != "" && p < level(c.AtLeast, strict):
			return false
		case c.AtMost != "" && p > level(c.AtMost, strict):
			return false
		}
	}
	return true
}

// Decide is table v1. targets are the legal targets and def the default among
// them; strict is the decision threshold. The first matching row wins:
//
//  1. The two pick questions chose different targets: hold, orders_disagree.
//  2. The target is the pick, at the lower of the two probabilities. The
//     default fits when its probability is at least FitsFloor; a default that
//     leaves the subject waiting does not fit when the subject is judged stuck
//     at the modest bar.
//  3. The pick is the default: commit it, followed.
//  4. The default fits: commit it, followed. A pick alone does not beat a
//     default judged to fit.
//  5. Acting would change approved intent (at or above the strict bar): the
//     move goes to the owner.
//  6. The pick is at or above its bar and the trigger's answers corroborate
//     it: commit it, departed.
//  7. Otherwise: hold, below_bar.
func Decide(def string, targets []Target, a jev.TransitionAnswers, strict float64) Verdict {
	if a.PickA.Choice != a.PickB.Choice {
		return Verdict{Action: ActionHold, Rule: "1:orders_disagree", Hold: HoldOrdersDisagree, P: math.Min(a.PickA.Confidence, a.PickB.Confidence)}
	}
	pick, p := a.PickA.Choice, math.Min(a.PickA.Confidence, a.PickB.Confidence)
	fits := a.Fit >= FitsFloor
	for _, t := range targets {
		if t.Name == def && t.Stay && a.Stuck >= ModestBar {
			fits = false
		}
	}
	switch {
	case pick == def:
		return Verdict{Action: ActionCommit, Target: def, Rule: "3:default_picked", P: p, Fits: fits}
	case fits:
		return Verdict{Action: ActionCommit, Target: def, Rule: "4:default_fits", P: a.Fit, Fits: true}
	case a.Intent >= strict:
		return Verdict{Action: ActionOwner, Target: pick, Rule: "5:would_change_intent", Hold: HoldIntent, P: p}
	}
	for _, t := range targets {
		if t.Name == pick && p >= barOf(t, strict) && corroborated(t, a.Own, strict) {
			return Verdict{Action: ActionCommit, Target: pick, Rule: "6:departure", Departed: true, P: p}
		}
	}
	return Verdict{Action: ActionHold, Target: pick, Rule: "7:below_bar", Hold: HoldBelowBar, P: p}
}

// confidenceBand returns the band the route span carries for a verdict.
func confidenceBand(v Verdict, asked bool, strict float64) string {
	if !asked {
		return ConfidenceNone
	}
	return ConfidenceBand(v.P, strict)
}

// describe is a sentence on Jev's answers for a notice, built by code from the
// answers alone.
func describe(a jev.TransitionAnswers, v Verdict) string {
	return fmt.Sprintf("default fits %.2f, picks %s (%.2f) and %s (%.2f), stuck %.2f, would change intent %.2f; table %s rule %s",
		a.Fit, a.PickA.Choice, a.PickA.Confidence, a.PickB.Choice, a.PickB.Confidence, a.Stuck, a.Intent, TableVersion, v.Rule)
}
