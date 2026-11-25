package kinds

import (
	"fmt"
	"strconv"
	"strings"
)

// ClimbFinal is a climb's final score as measured on one commit.
type ClimbFinal struct {
	// Commit is the commit the score belongs to.
	Commit string
	// Score is the primary metric's value. Valid is false for a measurement
	// that failed or was invalid, and Reason then says why; Score is unset.
	Score  float64
	Valid  bool
	Reason string
}

// ClimbKept is one attempt the climb kept: the commit it landed, its
// hypothesis, its score and the change from the best it started from.
type ClimbKept struct {
	Number     int
	Commit     string
	Hypothesis string
	Score      float64
	Change     float64
	// ChangePercent is nil when the best it started from was zero.
	ChangePercent *float64
}

// ClimbEarlier is an attempt the climb kept before an amendment changed what
// the evaluator measures and the climb was calibrated again. Its commit is on
// the branch, but its score was measured against another baseline and is not
// compared with the final.
type ClimbEarlier struct {
	Number       int
	Commit       string
	Hypothesis   string
	SpecRevision int
}

// ClimbResults is what a Hill Climb workstream's pull-request section is
// written from: the baseline, the final before and after the one conclusion
// rebase, and the kept attempts.
type ClimbResults struct {
	Goal           string
	Metric         string
	Unit           string
	Direction      string
	MinImprovement float64
	// Baseline is the calibrated primary score on the evaluator commit.
	Baseline       float64
	BaselineCommit string
	// Attempts counts every recorded attempt.
	Attempts int
	Kept     []ClimbKept
	// Earlier lists the attempts kept under an earlier calibration.
	Earlier []ClimbEarlier
	// Before is the final on the climb branch before the rebase, and After the
	// final measured again on the rebased commit.
	Before ClimbFinal
	After  ClimbFinal
	// Upstream names what the branch was rebased onto.
	UpstreamBranch string
	UpstreamCommit string
}

// EvaluatorOnly says the climb kept no attempt, so its branch holds only the
// evaluator.
func (r ClimbResults) EvaluatorOnly() bool { return len(r.Kept) == 0 && len(r.Earlier) == 0 }

func short(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func withUnit(v float64, unit string) string {
	if unit == "" {
		return number(v)
	}
	return number(v) + " " + unit
}

func signed(v float64) string {
	switch {
	case v < 0:
		return "−" + number(-v)
	case v > 0:
		return "+" + number(v)
	}
	return "0"
}

func changeText(from, to float64) string {
	d := to - from
	text := signed(d)
	if from != 0 {
		text += " (" + SignedPercent(d/absolute(from)*100) + ")"
	}
	return text
}

func absolute(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func finalCell(f ClimbFinal, baseline float64, unit string) (score, change string) {
	if !f.Valid {
		reason := strings.TrimSpace(f.Reason)
		if reason == "" {
			reason = "the measurement is invalid"
		}
		return "not measured: " + reason, ""
	}
	return withUnit(f.Score, unit), changeText(baseline, f.Score)
}

func arrow(baseline float64, f ClimbFinal) string {
	if !f.Valid {
		return number(baseline) + " → not measured"
	}
	return number(baseline) + " → " + number(f.Score)
}

func escapeCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// climbSection writes the pull-request section of a Hill Climb workstream: a
// results table with the baseline and the final before and after the rebase,
// then each kept attempt with its change from the best it started from.
func climbSection(r *ClimbResults) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Hill Climb results\n\n")
	better := r.Direction
	if better == "" {
		better = "lower"
	}
	fmt.Fprintf(&b, "Metric `%s`, %s is better; an attempt is kept when it beats the best by at least %s. %d %s recorded, %d kept.\n\n",
		r.Metric, better, withUnit(r.MinImprovement, r.Unit), r.Attempts, plural(r.Attempts, "attempt", "attempts"), len(r.Kept)+len(r.Earlier))
	b.WriteString("| Measured | Commit | Score | Change from baseline |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| Baseline | `%s` | %s | |\n", short(r.BaselineCommit), withUnit(r.Baseline, r.Unit))
	score, change := finalCell(r.Before, r.Baseline, r.Unit)
	fmt.Fprintf(&b, "| Final before the rebase | `%s` | %s | %s |\n", short(r.Before.Commit), escapeCell(score), change)
	score, change = finalCell(r.After, r.Baseline, r.Unit)
	fmt.Fprintf(&b, "| Final after the rebase | `%s` | %s | %s |\n\n", short(r.After.Commit), escapeCell(score), change)
	fmt.Fprintf(&b, "Baseline → final: %s before the rebase, %s after it", arrow(r.Baseline, r.Before), arrow(r.Baseline, r.After))
	if r.UpstreamCommit != "" {
		fmt.Fprintf(&b, ", rebased once onto `%s` at `%s`", r.UpstreamBranch, short(r.UpstreamCommit))
	}
	b.WriteString(".\n\n")
	if r.EvaluatorOnly() {
		b.WriteString("The climb kept no attempt, so this pull request holds only the evaluator and the files it reads.\n")
		return b.String()
	}
	if len(r.Earlier) > 0 {
		b.WriteString("Kept under an earlier calibration, before an amendment of the climb spec changed what the evaluator measures. They are on the branch, and their scores are not comparable with the table above:\n\n| Attempt | Commit | Hypothesis | Climb spec revision |\n|---|---|---|---|\n")
		for _, k := range r.Earlier {
			fmt.Fprintf(&b, "| %d | `%s` | %s | %d |\n", k.Number, short(k.Commit), escapeCell(k.Hypothesis), max(k.SpecRevision, 1))
		}
		b.WriteString("\n")
	}
	if len(r.Kept) == 0 {
		b.WriteString("The climb kept no attempt after its last calibration.\n")
		return b.String()
	}
	b.WriteString("Kept attempts:\n\n| Attempt | Commit | Hypothesis | Score | Delta |\n|---|---|---|---|---|\n")
	for _, k := range r.Kept {
		delta := signed(k.Change)
		if k.ChangePercent != nil {
			delta += " (" + SignedPercent(*k.ChangePercent) + ")"
		}
		fmt.Fprintf(&b, "| %d | `%s` | %s | %s | %s |\n", k.Number, short(k.Commit), escapeCell(k.Hypothesis), withUnit(k.Score, r.Unit), delta)
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
