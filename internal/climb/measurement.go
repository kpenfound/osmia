package climb

import (
	"fmt"
	"math"
	"slices"
)

// Measurement is the aggregate of the runs of one candidate or of
// calibration. It is valid only when every requested run scored; one failed
// or invalid run makes the whole measurement invalid, and no value is taken
// from the remaining runs.
type Measurement struct {
	// Repeats is how many runs were requested.
	Repeats int `json:"repeats"`
	// Runs holds every run that was made. A measurement that ended at its
	// first bad run holds fewer than Repeats.
	Runs   []Run  `json:"runs"`
	Valid  bool   `json:"valid"`
	Reason string `json:"reason,omitempty"`
	// FailedRun is the number of the run that made the measurement invalid.
	FailedRun int `json:"failed_run,omitempty"`
	// Score is the median of the primary metric over the runs, and Scores
	// holds its value in each run. Both are set only when the measurement is
	// valid.
	Score  float64   `json:"score"`
	Scores []float64 `json:"scores,omitempty"`
	// Spread is the range of the primary metric over the runs, its largest
	// value minus its smallest.
	Spread float64 `json:"spread"`
	// Metrics is the median of every required metric, and Units the unit each
	// reported. Both are set only when the measurement is valid.
	Metrics map[string]float64 `json:"metrics,omitempty"`
	Units   map[string]string  `json:"units,omitempty"`
}

// Aggregate combines the runs of one measurement. The measurement is valid
// when exactly repeats runs were made and every one scored with the same unit
// for each required metric; its value is then the median of each required
// metric, the mean of the two middle values when repeats is even.
func Aggregate(expect Expectations, repeats int, runs []Run) Measurement {
	m := Measurement{Repeats: repeats, Runs: runs}
	invalid := func(run int, format string, args ...any) Measurement {
		m.FailedRun, m.Reason = run, fmt.Sprintf(format, args...)
		return m
	}
	for _, run := range runs {
		switch run.Status {
		case RunScored:
		case RunInvalid:
			return invalid(run.Number, "run %d of %d was invalid: %s", run.Number, repeats, run.Reason)
		default:
			return invalid(run.Number, "run %d of %d failed: %s", run.Number, repeats, run.Reason)
		}
	}
	if len(runs) != repeats || repeats < 1 {
		return invalid(0, "%d of %d runs were made", len(runs), repeats)
	}
	required := expect.Required()
	values := make(map[string][]float64, len(required))
	units := make(map[string]string, len(required))
	for _, run := range runs {
		for _, x := range required {
			metric, ok := run.Parsed.Metrics[x.Name]
			if !ok {
				return invalid(run.Number, "run %d of %d lacks the metric %q", run.Number, repeats, x.Name)
			}
			if unit, seen := units[x.Name]; seen && unit != metric.Unit {
				return invalid(run.Number, "metric %q reports unit %q in run %d but %q before", x.Name, metric.Unit, run.Number, unit)
			}
			units[x.Name] = metric.Unit
			values[x.Name] = append(values[x.Name], metric.Value)
		}
	}
	m.Valid, m.Units = true, units
	m.Metrics = make(map[string]float64, len(required))
	for name, v := range values {
		m.Metrics[name] = Median(v)
	}
	m.Scores = values[expect.Primary.Name]
	m.Score = m.Metrics[expect.Primary.Name]
	lo, hi := slices.Min(m.Scores), slices.Max(m.Scores)
	m.Spread = hi - lo
	return m
}

// Median returns the middle of values, or the mean of the two middle ones
// when their number is even. It is zero for no values.
func Median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return sorted[mid-1] + (sorted[mid]-sorted[mid-1])/2
}

// atLeast reports whether a is at least b, allowing for the rounding that
// arithmetic on measured values leaves, so an improvement that equals the
// minimum improvement counts as reaching it.
func atLeast(a, b float64) bool {
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return a >= b-1e-12*scale
}
