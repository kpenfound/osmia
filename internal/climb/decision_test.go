package climb

import (
	"strings"
	"testing"
)

func valid(score float64, metrics map[string]float64) Measurement {
	m := map[string]float64{"wall_ms": score}
	for k, v := range metrics {
		m[k] = v
	}
	return Measurement{Repeats: 1, Valid: true, Score: score, Metrics: m, Runs: []Run{{Status: RunScored}}}
}

func ptr(v float64) *float64 { return &v }

func TestDecideKeepsOnlyWhenEveryConditionHolds(t *testing.T) {
	t.Parallel()
	base := Candidate{Measurement: valid(90, map[string]float64{"peak_rss_mb": 50}), Best: 100, Direction: Lower, MinImprovement: 5, ChecksGreen: true, Limits: []Limit{{Metric: "peak_rss_mb", Max: ptr(64)}}}
	d := Decide(base)
	if !d.Keep || d.Cause != "" || d.Reason != "" || d.Change != -10 || d.Improvement != 10 {
		t.Fatalf("every condition holds: %+v", d)
	}

	for _, c := range []struct {
		name   string
		mutate func(*Candidate)
		cause  Cause
		reason string
	}{
		{"invalid measurement that would otherwise win", func(c *Candidate) {
			c.Measurement = Measurement{Repeats: 3, Reason: "run 2 of 3 failed: boom", FailedRun: 2, Runs: []Run{{Status: RunScored}, {Status: RunFailed}}}
		}, CauseInvalid, "run 2 of 3 failed: boom"},
		{"guardrails not green", func(c *Candidate) { c.ChecksGreen, c.ChecksDetail = false, "lint failed" }, CauseGuardrails, "lint failed"},
		{"guardrails not green without detail", func(c *Candidate) { c.ChecksGreen = false }, CauseGuardrails, "guardrail check did not pass"},
		{"secondary limit exceeded", func(c *Candidate) { c.Measurement = valid(90, map[string]float64{"peak_rss_mb": 65}) }, CauseLimits, "peak_rss_mb is 65, above its limit 64"},
		{"secondary below its minimum", func(c *Candidate) {
			c.Limits = []Limit{{Metric: "peak_rss_mb", Min: ptr(60)}}
		}, CauseLimits, "below its limit 60"},
		{"limited metric not measured", func(c *Candidate) { c.Limits = []Limit{{Metric: "other", Max: ptr(1)}} }, CauseLimits, "other has no measured value"},
		{"improvement below the minimum", func(c *Candidate) { c.Measurement = valid(96, map[string]float64{"peak_rss_mb": 50}) }, CauseNoImprovement, "short of the minimum improvement 5"},
		{"no change", func(c *Candidate) { c.Measurement = valid(100, map[string]float64{"peak_rss_mb": 50}) }, CauseNoImprovement, "improves on the best 100 by 0"},
		{"worse than the best", func(c *Candidate) { c.Measurement = valid(120, map[string]float64{"peak_rss_mb": 50}) }, CauseNoImprovement, "by -20"},
		{"direction wrong way round", func(c *Candidate) { c.Direction = Higher }, CauseNoImprovement, "by -10"},
	} {
		in := base
		c.mutate(&in)
		d := Decide(in)
		if d.Keep || d.Cause != c.cause || !strings.Contains(d.Reason, c.reason) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

func TestDecideCoversBothDirectionsAndAnImprovementEqualToTheMinimum(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		direction Direction
		best      float64
		score     float64
		min       float64
		keep      bool
	}{
		{"lower, equal to the minimum", Lower, 100, 95, 5, true},
		{"lower, just short", Lower, 100, 95.5, 5, false},
		{"lower, beyond", Lower, 100, 80, 5, true},
		{"higher, equal to the minimum", Higher, 1000, 1050, 50, true},
		{"higher, just short", Higher, 1000, 1049, 50, false},
		{"higher, beyond", Higher, 1000, 1500, 50, true},
		{"higher, regression", Higher, 1000, 900, 50, false},
		{"lower, equal despite rounding", Lower, 0.3, 0.1, 0.2, true},
		{"higher, equal despite rounding", Higher, 0.1, 0.3, 0.2, true},
		{"lower, negative scores", Lower, -10, -15, 5, true},
		{"lower, near zero best", Lower, 0, -1, 1, true},
	} {
		d := Decide(Candidate{Measurement: valid(c.score, nil), Best: c.best, Direction: c.direction, MinImprovement: c.min, ChecksGreen: true})
		if d.Keep != c.keep {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

func TestDecideListsEveryFailingConditionOfAValidMeasurement(t *testing.T) {
	t.Parallel()
	d := Decide(Candidate{Measurement: valid(99, map[string]float64{"peak_rss_mb": 70}), Best: 100, Direction: Lower, MinImprovement: 5, ChecksGreen: false, ChecksDetail: "tests failed", Limits: []Limit{{Metric: "peak_rss_mb", Max: ptr(64)}}})
	if d.Keep || d.Cause != CauseGuardrails {
		t.Fatalf("%+v", d)
	}
	for _, want := range []string{"tests failed", "peak_rss_mb is 70", "short of the minimum improvement"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason %q lacks %q", d.Reason, want)
		}
	}
}

func TestDecideNeverKeepsWhenTheMetricIsMisconfigured(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]Candidate{
		"no direction":         {Measurement: valid(1, nil), Best: 100, MinImprovement: 5, ChecksGreen: true},
		"zero minimum":         {Measurement: valid(1, nil), Best: 100, Direction: Lower, ChecksGreen: true},
		"negative minimum":     {Measurement: valid(1, nil), Best: 100, Direction: Lower, MinImprovement: -1, ChecksGreen: true},
		"zero-value candidate": {},
	} {
		if d := Decide(c); d.Keep {
			t.Errorf("%s was kept: %+v", name, d)
		}
	}
}

func TestLimitVerifyTreatsTheBoundsAsInclusive(t *testing.T) {
	t.Parallel()
	l := Limit{Metric: "m", Min: ptr(1), Max: ptr(2)}
	for value, ok := range map[float64]bool{0.9: false, 1: true, 1.5: true, 2: true, 2.1: false} {
		if got := l.Verify(value) == ""; got != ok {
			t.Errorf("value %v: met=%v", value, got)
		}
	}
	if (Limit{Metric: "m"}).Verify(1e9) != "" {
		t.Fatal("a limit without bounds refused a value")
	}
}
