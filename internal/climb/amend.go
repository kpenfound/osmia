package climb

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// The fields of a climb spec an amendment can change, as SpecChange names
// them. The first group depends on calibration: a change to any of them
// leaves the baseline, the units, the noise band, the frozen protected paths
// or the best the attempts are compared with measuring something else. The
// second group applies in place.
const (
	FieldEvaluator      = "evaluator"
	FieldMetricName     = "metric.name"
	FieldMetricDir      = "metric.direction"
	FieldMinImprovement = "metric.min_improvement"
	FieldRepeats        = "repeats"
	FieldSecondary      = "guardrails.limits metrics"
	FieldProtected      = "protected paths"

	FieldGoal   = "goal"
	FieldStop   = "stop rules"
	FieldChecks = "guardrails.checks"
	FieldBounds = "guardrails.limits bounds"
)

// SpecChange classifies what differs between two climb specs. Calibration
// lists the changed fields a calibration depends on and InPlace the changed
// fields that apply without calibrating again: the goal, the stop rules, the
// guardrail checks and the bounds of the secondary limits.
type SpecChange struct {
	Calibration []string `json:"calibration,omitempty"`
	InPlace     []string `json:"in_place,omitempty"`
}

// Recalibrates reports that a field a calibration depends on changed.
func (c SpecChange) Recalibrates() bool { return len(c.Calibration) > 0 }

// None reports that no field changed.
func (c SpecChange) None() bool { return len(c.Calibration) == 0 && len(c.InPlace) == 0 }

// Words names the fields that force a new calibration.
func (c SpecChange) Words() string { return strings.Join(c.Calibration, ", ") }

// Compare classifies every field that differs between the climb spec a record
// was made under and the one now sealed. It is plain arithmetic over the two
// specs: the same two specs always classify the same way.
func Compare(old, current Spec) SpecChange {
	var c SpecChange
	if old.Evaluator != current.Evaluator {
		c.Calibration = append(c.Calibration, FieldEvaluator)
	}
	if old.Metric.Name != current.Metric.Name {
		c.Calibration = append(c.Calibration, FieldMetricName)
	}
	if old.Metric.Direction != current.Metric.Direction {
		c.Calibration = append(c.Calibration, FieldMetricDir)
	}
	if old.Metric.MinImprovement != current.Metric.MinImprovement {
		c.Calibration = append(c.Calibration, FieldMinImprovement)
	}
	if old.Repeats != current.Repeats {
		c.Calibration = append(c.Calibration, FieldRepeats)
	}
	if !reflect.DeepEqual(secondaryMetrics(old), secondaryMetrics(current)) {
		c.Calibration = append(c.Calibration, FieldSecondary)
	}
	if !slices.Equal(protectedSet(old), protectedSet(current)) {
		c.Calibration = append(c.Calibration, FieldProtected)
	}
	if strings.TrimSpace(old.Goal) != strings.TrimSpace(current.Goal) {
		c.InPlace = append(c.InPlace, FieldGoal)
	}
	if !reflect.DeepEqual(old.Stop, current.Stop) {
		c.InPlace = append(c.InPlace, FieldStop)
	}
	if !slices.Equal(old.Guardrails.Checks, current.Guardrails.Checks) {
		c.InPlace = append(c.InPlace, FieldChecks)
	}
	if boundsChanged(old, current) {
		c.InPlace = append(c.InPlace, FieldBounds)
	}
	return c
}

// boundsChanged reports a limit that both climb specs set on the same metric
// with other bounds. A metric one of them does not limit is a change of the
// secondary metrics, which calibration depends on.
func boundsChanged(old, current Spec) bool {
	before, after := limitBounds(old), limitBounds(current)
	for metric, b := range before {
		if a, both := after[metric]; both && !reflect.DeepEqual(a, b) {
			return true
		}
	}
	return false
}

type secondary struct {
	Metric    string
	Direction Direction
}

func secondaryMetrics(s Spec) []secondary {
	out := make([]secondary, 0, len(s.Guardrails.Limits))
	for _, l := range s.Guardrails.Limits {
		out = append(out, secondary{Metric: l.Metric, Direction: l.Direction})
	}
	slices.SortFunc(out, func(a, b secondary) int { return strings.Compare(a.Metric, b.Metric) })
	return out
}

type bounds struct {
	Min, Max *float64
}

func limitBounds(s Spec) map[string]bounds {
	out := map[string]bounds{}
	for _, l := range s.Guardrails.Limits {
		out[l.Metric] = bounds{Min: l.Min, Max: l.Max}
	}
	return out
}

// protectedSet is the sorted set of paths a climb spec names to freeze, the
// fixtures and the additions together, since the gate and the approved copy
// treat them alike.
func protectedSet(s Spec) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(slices.Clone(s.Protected.Fixtures), s.Protected.Add...) {
		clean, err := CleanPath(p)
		if err != nil {
			clean = p
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	slices.Sort(out)
	return out
}

// SpecReader returns the climb spec recorded at a revision of the workstream's
// spec slot.
type SpecReader func(stream config.WorkstreamID, revision int) (Spec, error)

// ErrNoSpecReader refuses to compare two climb spec revisions without a way to
// read the older one.
var ErrNoSpecReader = errors.New("the climb spec revision a record was made under cannot be read")

// Drift compares the climb spec a record was made under, at revision pinned,
// with the sealed climb spec at revision current. A record that pins no
// revision, and one made under the sealed revision, has not drifted.
func Drift(read SpecReader, stream config.WorkstreamID, pinned int, sealed Spec, current int) (SpecChange, error) {
	if pinned == 0 || pinned == current {
		return SpecChange{}, nil
	}
	if read == nil {
		return SpecChange{}, ErrNoSpecReader
	}
	old, err := read(stream, pinned)
	if err != nil {
		return SpecChange{}, fmt.Errorf("climb spec revision %d: %w", pinned, err)
	}
	return Compare(old, sealed), nil
}
