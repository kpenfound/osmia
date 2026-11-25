package climb

import (
	"errors"
	"fmt"
)

// Expectation is what the climb spec requires of one metric. Unit stays empty
// until calibration records it, and from then on a run must report that unit.
type Expectation struct {
	Name      string    `json:"name"`
	Direction Direction `json:"direction"`
	Unit      string    `json:"unit,omitempty"`
}

// Expectations lists the metrics a run must report: the primary metric and
// every secondary metric a guardrail limits.
type Expectations struct {
	Primary   Expectation   `json:"primary"`
	Secondary []Expectation `json:"secondary,omitempty"`
}

// Validate reports a malformed set of expectations.
func (e Expectations) Validate() error {
	seen := map[string]bool{}
	for i, x := range e.Required() {
		switch {
		case x.Name == "":
			return errors.New("a required metric has no name")
		case !x.Direction.Valid():
			return fmt.Errorf("metric %q has direction %q, not higher or lower", x.Name, x.Direction)
		case seen[x.Name]:
			return fmt.Errorf("metric %q is required twice (position %d)", x.Name, i)
		}
		seen[x.Name] = true
	}
	return nil
}

// Required returns the primary expectation followed by the secondary ones.
func (e Expectations) Required() []Expectation {
	return append([]Expectation{e.Primary}, e.Secondary...)
}

// WithUnits returns the expectations with the calibrated unit of each metric
// that units names.
func (e Expectations) WithUnits(units map[string]string) Expectations {
	out := Expectations{Primary: e.Primary}
	out.Primary.Unit = units[e.Primary.Name]
	for _, x := range e.Secondary {
		x.Unit = units[x.Name]
		out.Secondary = append(out.Secondary, x)
	}
	return out
}

// Check returns why a conforming output still fails the expectations: it
// omits a required metric, reports another direction, or reports another
// unit than the calibrated one. It returns nil when every required metric is
// present and agrees.
func (e Expectations) Check(out Output) error {
	for i, x := range e.Required() {
		m, ok := out.Metrics[x.Name]
		if !ok {
			if i == 0 {
				return fmt.Errorf("the output lacks the primary metric %q", x.Name)
			}
			return fmt.Errorf("the output lacks the secondary metric %q that a guardrail limits", x.Name)
		}
		if m.Direction != x.Direction {
			return fmt.Errorf("metric %q reports direction %q, but the climb spec says %q", x.Name, m.Direction, x.Direction)
		}
		if x.Unit != "" && m.Unit != x.Unit {
			return fmt.Errorf("metric %q reports unit %q, but calibration recorded %q", x.Name, m.Unit, x.Unit)
		}
	}
	return nil
}
