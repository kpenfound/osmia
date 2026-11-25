package climb

import (
	"math"
	"strings"
	"testing"
)

func scoredRun(n int, wall, rss float64) Run {
	return Run{Number: n, Status: RunScored, Parsed: &Output{Schema: 1, Valid: true, Metrics: map[string]Metric{
		"wall_ms":     {Value: wall, Unit: "ms", Direction: Lower},
		"peak_rss_mb": {Value: rss, Unit: "MB", Direction: Lower},
	}}}
}

func higherExpectations() Expectations {
	return Expectations{Primary: Expectation{Name: "throughput", Direction: Higher}}
}

func throughputRun(n int, value float64) Run {
	return Run{Number: n, Status: RunScored, Parsed: &Output{Schema: 1, Valid: true, Metrics: map[string]Metric{"throughput": {Value: value, Unit: "ops/s", Direction: Higher}}}}
}

func TestMedian(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		values []float64
		want   float64
	}{
		{nil, 0},
		{[]float64{7}, 7},
		{[]float64{3, 1, 2}, 2},
		{[]float64{9, 1, 5, 100, 4}, 5},
		{[]float64{4, 1}, 2.5},
		{[]float64{10, 1, 2, 9}, 5.5},
		{[]float64{-3, -1, -2}, -2},
	} {
		in := append([]float64(nil), c.values...)
		if got := Median(c.values); got != c.want {
			t.Errorf("median of %v is %v, want %v", c.values, got, c.want)
		}
		for i := range in {
			if in[i] != c.values[i] {
				t.Errorf("median reordered its input %v", c.values)
			}
		}
	}
}

func TestAggregateTakesTheMedianOfEveryRequiredMetric(t *testing.T) {
	t.Parallel()
	m := Aggregate(expectations(), 3, []Run{scoredRun(1, 120, 30), scoredRun(2, 100, 10), scoredRun(3, 110, 20)})
	if !m.Valid || m.Score != 110 || m.Metrics["peak_rss_mb"] != 20 || m.Spread != 20 || m.Units["wall_ms"] != "ms" || len(m.Scores) != 3 {
		t.Fatalf("%+v", m)
	}
	one := Aggregate(expectations(), 1, []Run{scoredRun(1, 100, 10)})
	if !one.Valid || one.Score != 100 || one.Spread != 0 {
		t.Fatalf("one run: %+v", one)
	}
	even := Aggregate(expectations(), 2, []Run{scoredRun(1, 100, 10), scoredRun(2, 110, 10)})
	if !even.Valid || even.Score != 105 || even.Spread != 10 {
		t.Fatalf("two runs: %+v", even)
	}
}

func TestAggregateIsCorrectForAHigherIsBetterMetric(t *testing.T) {
	t.Parallel()
	m := Aggregate(higherExpectations(), 3, []Run{throughputRun(1, 900), throughputRun(2, 1100), throughputRun(3, 1000)})
	if !m.Valid || m.Score != 1000 || m.Spread != 200 || m.Units["throughput"] != "ops/s" {
		t.Fatalf("%+v", m)
	}
}

func TestAggregateIsInvalidWhenAnyRunFailedOrWasInvalid(t *testing.T) {
	t.Parallel()
	failed := Run{Number: 2, Status: RunFailed, Reason: "the evaluator exited 1"}
	invalid := Run{Number: 2, Status: RunInvalid, Reason: "wrong answer"}
	for name, c := range map[string]struct {
		run  Run
		want string
	}{
		"failed":  {failed, "run 2 of 3 failed: the evaluator exited 1"},
		"invalid": {invalid, "run 2 of 3 was invalid: wrong answer"},
	} {
		m := Aggregate(expectations(), 3, []Run{scoredRun(1, 1, 1), c.run, scoredRun(3, 1, 1)})
		if m.Valid || m.FailedRun != 2 || m.Reason != c.want || m.Score != 0 || m.Scores != nil || m.Metrics != nil || m.Units != nil {
			t.Errorf("%s: %+v", name, m)
		}
	}
}

func TestAggregateIsInvalidWhenRunsAreMissingOrDisagreeOnAUnit(t *testing.T) {
	t.Parallel()
	if m := Aggregate(expectations(), 3, []Run{scoredRun(1, 1, 1), scoredRun(2, 1, 1)}); m.Valid || !strings.Contains(m.Reason, "2 of 3 runs were made") {
		t.Fatalf("missing run: %+v", m)
	}
	if m := Aggregate(expectations(), 0, nil); m.Valid {
		t.Fatalf("no runs: %+v", m)
	}
	odd := scoredRun(2, 1, 1)
	odd.Parsed.Metrics["wall_ms"] = Metric{Value: 1, Unit: "s", Direction: Lower}
	if m := Aggregate(expectations(), 2, []Run{scoredRun(1, 1, 1), odd}); m.Valid || !strings.Contains(m.Reason, `unit "s" in run 2 but "ms" before`) {
		t.Fatalf("unit changes between runs: %+v", m)
	}
	lacking := scoredRun(2, 1, 1)
	delete(lacking.Parsed.Metrics, "peak_rss_mb")
	if m := Aggregate(expectations(), 2, []Run{scoredRun(1, 1, 1), lacking}); m.Valid || !strings.Contains(m.Reason, `lacks the metric "peak_rss_mb"`) {
		t.Fatalf("metric missing in a run: %+v", m)
	}
}

func TestCalibrateEstablishesBaselineUnitsAndSpread(t *testing.T) {
	t.Parallel()
	lower := Calibrate(Aggregate(expectations(), 3, []Run{scoredRun(1, 100, 10), scoredRun(2, 103, 10), scoredRun(3, 101, 12)}), 5)
	if lower.Verdict != Calibrated || lower.Baseline != 101 || lower.Spread != 3 || lower.Units["wall_ms"] != "ms" || lower.Units["peak_rss_mb"] != "MB" || lower.Metrics["peak_rss_mb"] != 10 || lower.Reason != "" {
		t.Fatalf("lower: %+v", lower)
	}
	higher := Calibrate(Aggregate(higherExpectations(), 3, []Run{throughputRun(1, 1000), throughputRun(2, 1040), throughputRun(3, 1010)}), 50)
	if higher.Verdict != Calibrated || higher.Baseline != 1010 || higher.Spread != 40 || higher.Units["throughput"] != "ops/s" {
		t.Fatalf("higher: %+v", higher)
	}
}

func TestCalibrateHoldsAsTooNoisyWhenTheSpreadReachesTheMinimumImprovement(t *testing.T) {
	t.Parallel()
	runs := func(v ...float64) []Run {
		var out []Run
		for i, x := range v {
			out = append(out, throughputRun(i+1, x))
		}
		return out
	}
	for _, c := range []struct {
		name string
		runs []Run
		min  float64
		want Verdict
	}{
		{"spread below the minimum", runs(100, 104, 102), 5, Calibrated},
		{"spread equal to the minimum", runs(100, 105, 102), 5, TooNoisy},
		{"spread above the minimum", runs(100, 120, 102), 5, TooNoisy},
		{"one run has no spread", runs(100), 5, Calibrated},
	} {
		got := Calibrate(Aggregate(higherExpectations(), len(c.runs), c.runs), c.min)
		if got.Verdict != c.want {
			t.Errorf("%s: %+v", c.name, got)
		}
		if got.Verdict == TooNoisy && (got.Baseline == 0 || got.Reason == "") {
			t.Errorf("%s: a noisy calibration still reports its baseline and says why: %+v", c.name, got)
		}
	}
	lowerRuns := []Run{scoredRun(1, 100, 1), scoredRun(2, 110, 1), scoredRun(3, 105, 1)}
	if got := Calibrate(Aggregate(expectations(), 3, lowerRuns), 10); got.Verdict != TooNoisy || got.Spread != 10 {
		t.Fatalf("lower direction: %+v", got)
	}
}

func TestCalibrateIsBrokenWhenARunFailedAndHasNoBaseline(t *testing.T) {
	t.Parallel()
	bad := Run{Number: 2, Status: RunFailed, Reason: "the output lacks the primary metric"}
	c := Calibrate(Aggregate(expectations(), 3, []Run{scoredRun(1, 100, 1), bad}), 5)
	if c.Verdict != BrokenMetric || c.Baseline != 0 || c.Spread != 0 || c.Metrics != nil || c.Units != nil || !strings.Contains(c.Reason, "run 2 of 3 failed") {
		t.Fatalf("%+v", c)
	}
	invalid := Run{Number: 1, Status: RunInvalid, Reason: "wrong"}
	if c := Calibrate(Aggregate(expectations(), 3, []Run{invalid}), 5); c.Verdict != BrokenMetric {
		t.Fatalf("an invalid run: %+v", c)
	}
	good := Aggregate(expectations(), 1, []Run{scoredRun(1, 100, 1)})
	for _, min := range []float64{0, -1, math.NaN()} {
		if c := Calibrate(good, min); c.Verdict != BrokenMetric || c.Baseline != 0 {
			t.Errorf("minimum improvement %v: %+v", min, c)
		}
	}
}
