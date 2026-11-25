package kinds

import (
	"strings"
	"testing"
)

func ptr(v float64) *float64 { return &v }

func resultsOf() ClimbResults {
	return ClimbResults{
		Metric: "wall_ms", Unit: "ms", Direction: "lower", MinImprovement: 5,
		Baseline: 101, BaselineCommit: "evaluator0001abcd", Attempts: 3,
		Kept:   []ClimbKept{{Number: 1, Commit: "kept0000001abcd", Hypothesis: "cache | the parsed\ncorpus", Score: 90, Change: -11, ChangePercent: ptr(-10.89)}},
		Before: ClimbFinal{Commit: "kept0000001abcd", Score: 90, Valid: true},
		After:  ClimbFinal{Commit: "rebased00001abcd", Score: 91, Valid: true},
		UpstreamBranch: "main", UpstreamCommit: "upstream0001abcd",
	}
}

func TestTheHillClimbSectionShowsBaselineToFinalBeforeAndAfterTheRebaseAndEachKeptAttempt(t *testing.T) {
	k, _ := Lookup(HillClimb)
	got := k.PullRequestSection(PullRequest{Workstream: "w", Climb: ptr2(resultsOf())})
	for _, want := range []string{
		"## Hill Climb results",
		"Metric `wall_ms`, lower is better; an attempt is kept when it beats the best by at least 5 ms. 3 attempts recorded, 1 kept.",
		"| Baseline | `evaluator0` | 101 ms | |",
		"| Final before the rebase | `kept000000` | 90 ms | \u221211 (\u221210.9%) |",
		"| Final after the rebase | `rebased000` | 91 ms | \u221210 (\u22129.9%) |",
		"Baseline \u2192 final: 101 \u2192 90 before the rebase, 101 \u2192 91 after it, rebased once onto `main` at `upstream00`.",
		"| 1 | `kept000000` | cache \\| the parsed corpus | 90 ms | \u221211 (\u221210.9%) |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the section lacks %q:\n%s", want, got)
		}
	}
}

func ptr2(r ClimbResults) *ClimbResults { return &r }

func TestTheHillClimbSectionOfAClimbThatKeptNothingSaysItHoldsOnlyTheEvaluator(t *testing.T) {
	r := resultsOf()
	r.Kept = nil
	r.Before = ClimbFinal{Commit: "evaluator0001abcd", Score: 101, Valid: true}
	got := climbSection(&r)
	if !strings.Contains(got, "holds only the evaluator") || strings.Contains(got, "Kept attempts:") || !strings.Contains(got, "101 \u2192 101 before the rebase") {
		t.Fatalf("the section:\n%s", got)
	}
	if !r.EvaluatorOnly() {
		t.Fatal("a climb with no kept attempt is evaluator-only")
	}
}

func TestTheHillClimbSectionNamesAFinalThatCouldNotBeMeasuredAfterTheRebase(t *testing.T) {
	r := resultsOf()
	r.After = ClimbFinal{Commit: "rebased00001abcd", Reason: "the oracle disagrees"}
	got := climbSection(&r)
	if !strings.Contains(got, "not measured: the oracle disagrees") || !strings.Contains(got, "101 \u2192 not measured after it") {
		t.Fatalf("the section:\n%s", got)
	}
}

func TestTheHillClimbSectionReadsAHigherIsBetterMetricWithoutNegatingIt(t *testing.T) {
	r := resultsOf()
	r.Metric, r.Unit, r.Direction, r.Baseline = "throughput", "ops/s", "higher", 200
	r.Kept = []ClimbKept{{Number: 1, Commit: "kept0000001abcd", Hypothesis: "batch", Score: 250, Change: 50, ChangePercent: ptr(25)}}
	r.Before = ClimbFinal{Commit: "kept0000001abcd", Score: 250, Valid: true}
	r.After = ClimbFinal{Commit: "rebased00001abcd", Score: 240, Valid: true}
	got := climbSection(&r)
	for _, want := range []string{"higher is better", "| Final before the rebase | `kept000000` | 250 ops/s | +50 (+25.0%) |", "| Final after the rebase | `rebased000` | 240 ops/s | +40 (+20.0%) |"} {
		if !strings.Contains(got, want) {
			t.Errorf("the section lacks %q:\n%s", want, got)
		}
	}
}

func TestFeatureAndAClimbWithNoResultsHaveNoSection(t *testing.T) {
	f, _ := Lookup(Feature)
	if got := f.PullRequestSection(PullRequest{Workstream: "w"}); got != "" {
		t.Fatalf("a feature's section: %q", got)
	}
	k, _ := Lookup(HillClimb)
	if got := k.PullRequestSection(PullRequest{Workstream: "w"}); got != "" {
		t.Fatalf("a climb that has not concluded has a section: %q", got)
	}
}
