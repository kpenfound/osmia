package climb

import (
	"fmt"
	"strings"
)

// Rejection is what the reviewer said about the last attempt when it
// rejected a kept candidate.
type Rejection struct {
	Attempt int
	Notes   string
}

// Brief is what a mason turn of an attempt carries beside the sealed climb
// spec and the unit's task: where the climb stands, the log of the attempts so
// far, the steering notes and the reviewer's notes from a rejected attempt.
type Brief struct {
	Number   int
	Parent   string
	Spec     Spec
	Baseline float64
	Best     float64
	Units    map[string]string
	Noise    float64
	// Protected lists the frozen paths an attempt must not touch.
	Protected []string
	Log       []Attempt
	// Earlier counts the attempts recorded before the climb was calibrated
	// again after an amendment. Their scores were measured against another
	// baseline, so the log does not list them.
	Earlier  int
	Steering []Note
	// Delivered is how many of the steering notes earlier attempts carried;
	// the others are new to this attempt.
	Delivered int
	Rejection *Rejection
}

// BriefOf assembles the brief of attempt n from the records. Only the
// attempts of the approval's epoch are the log and the best: an earlier
// attempt was measured against another baseline.
func BriefOf(n int, parent string, spec Spec, approval ApprovalRecord, all []Attempt, steering Steering, carried int) Brief {
	attempts := InEpoch(all, approval.EpochNumber())
	b := Brief{Number: n, Parent: parent, Spec: spec, Baseline: approval.Baseline, Units: approval.Units, Noise: approval.Noise, Protected: approval.Protected,
		Log: attempts, Earlier: len(all) - len(attempts), Steering: steering.Notes, Delivered: carried}
	b.Best, _ = Best(approval.Baseline, attempts)
	if len(attempts) > 0 {
		if last := attempts[len(attempts)-1]; last.Cause == CauseRejected && strings.TrimSpace(last.ReviewNotes) != "" {
			b.Rejection = &Rejection{Attempt: last.Number, Notes: last.ReviewNotes}
		}
	}
	return b
}

// Render returns the brief as the text a turn request carries.
func (b Brief) Render() string {
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	unit := b.Units[b.Spec.Metric.Name]
	line("# Attempt %d of the climb", b.Number)
	line("")
	line("You are the mason of this whole climb: one thread, one attempt per turn. This turn is attempt %d. Your workspace was built fresh from the current best, commit %s.", b.Number, b.Parent)
	line("")
	line("## Where the climb stands")
	line("- primary metric: %s, %s is better, measured in %s", b.Spec.Metric.Name, b.Spec.Metric.Direction, orWord(unit, "its calibrated unit"))
	line("- baseline %v, current best %v, run-to-run noise %v", b.Baseline, b.Best, b.Noise)
	line("- an attempt is kept only when the service's own measurement beats the current best by at least %v, the guardrails are green and the secondary limits are met; then a reviewer reads it for metric gaming, charter breaches and scope before it lands", b.Spec.Metric.MinImprovement)
	if b.Spec.Stop.Target != nil {
		line("- target score %v", *b.Spec.Stop.Target)
	}
	line("")
	line("## Rules of an attempt")
	line("- You may run the evaluator %q in your own sandbox to learn, but only the service's measurement counts, and you never decide whether a score improved: Go alone decides whether an attempt is kept, by arithmetic over that measurement.", b.Spec.Evaluator)
	line("- These paths are frozen. An attempt that changes any of them is discarded before it is measured: %s.", strings.Join(b.Protected, ", "))
	line("- Make one change worth measuring. Do not game the metric: the reviewer rejects a change that improves the score without improving what the climb is for.")
	line("- When your change is ready, call done with your hypothesis, the change you made and the effect you expect, then end your turn. If you have no change worth measuring, leave the workspace as it is and say so.")
	if b.Rejection != nil {
		line("")
		line("## The reviewer rejected attempt %d", b.Rejection.Attempt)
		line("%s", strings.TrimSpace(b.Rejection.Notes))
	}
	if len(b.Steering) > 0 {
		line("")
		line("## Steering notes from the chief of staff")
		for _, n := range b.Steering {
			mark := ""
			if n.Number > b.Delivered {
				mark = " (new)"
			}
			line("%d. %s%s", n.Number, strings.TrimSpace(n.Text), mark)
		}
	}
	line("")
	line("## Attempt log")
	if b.Earlier > 0 {
		line("%d earlier %s ran before the climb spec changed and the evaluator was calibrated again. Their scores were measured against another baseline, so they are not listed and say nothing about the current best.", b.Earlier, map[bool]string{true: "attempt", false: "attempts"}[b.Earlier == 1])
	}
	if len(b.Log) == 0 {
		line("No attempt has been recorded yet.")
	}
	for _, a := range b.Log {
		line("- attempt %d: %s", a.Number, summarize(a))
	}
	return w.String()
}

func orWord(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func summarize(a Attempt) string {
	var parts []string
	switch a.Outcome {
	case OutcomeKept:
		parts = append(parts, "kept")
	default:
		parts = append(parts, "discarded ("+string(a.Cause)+")")
	}
	if a.Scores != nil {
		parts = append(parts, fmt.Sprintf("score %v", a.Scores.Primary))
	}
	if a.Change != nil {
		parts = append(parts, fmt.Sprintf("change %+v", *a.Change))
	}
	if h := strings.TrimSpace(a.Hypothesis); h != "" {
		parts = append(parts, "hypothesis: "+oneLine(h))
	}
	if r := strings.TrimSpace(a.Reason); r != "" && a.Outcome != OutcomeKept {
		parts = append(parts, "why: "+oneLine(r))
	}
	return strings.Join(parts, "; ")
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 240 {
		return string(r[:240]) + "…"
	}
	return s
}
