package climb

import "fmt"

// Verdict is how calibration ended.
type Verdict string

const (
	// Calibrated means every run scored and the spread is below the minimum
	// improvement, so the metric can be climbed.
	Calibrated Verdict = "calibrated"
	// BrokenMetric means a run failed or was invalid, so the metric, not the
	// code, is broken. No baseline exists.
	BrokenMetric Verdict = "broken-metric"
	// TooNoisy means every run scored but the run-to-run spread reaches the
	// minimum improvement, so an improvement could not be told from noise.
	TooNoisy Verdict = "too-noisy"
)

// Calibration is what calibration runs establish.
type Calibration struct {
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason,omitempty"`
	// Baseline is the median of the primary metric, and Metrics the median
	// of each required metric. They are set unless the metric is broken.
	Baseline float64            `json:"baseline"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	// Units is the unit each required metric reported. Every later run must
	// report the same ones.
	Units map[string]string `json:"units,omitempty"`
	// Spread is the range of the primary metric over the calibration runs.
	Spread      float64     `json:"spread"`
	Measurement Measurement `json:"measurement"`
}

// Calibrate derives the calibration verdict from the aggregate of the
// calibration runs. A failed or invalid run, or an invalid minimum
// improvement, gives the broken-metric verdict and no baseline. A spread at or
// above the minimum improvement gives the too-noisy verdict.
func Calibrate(m Measurement, minImprovement float64) Calibration {
	c := Calibration{Measurement: m}
	switch {
	case !m.Valid:
		c.Verdict, c.Reason = BrokenMetric, m.Reason
	case !(minImprovement > 0):
		c.Verdict, c.Reason = BrokenMetric, fmt.Sprintf("the minimum improvement %v is not positive", minImprovement)
	default:
		c.Baseline, c.Metrics, c.Units, c.Spread = m.Score, m.Metrics, m.Units, m.Spread
		if atLeast(m.Spread, minImprovement) {
			c.Verdict = TooNoisy
			c.Reason = fmt.Sprintf("the spread %v between %d calibration runs is not below the minimum improvement %v", m.Spread, len(m.Runs), minImprovement)
		} else {
			c.Verdict = Calibrated
		}
	}
	return c
}
