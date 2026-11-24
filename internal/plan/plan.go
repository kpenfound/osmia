package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Version is the plan.json schema version Parse reads and Encode writes.
const Version = 2

// ErrUnsupportedVersion reports a plan.json whose version is missing or not
// one Parse reads.
var ErrUnsupportedVersion = errors.New("unsupported plan version")

// Plan is the content of plan.json: the directed graph of units.
type Plan struct {
	Version int    `json:"version"`
	Units   []Unit `json:"units"`
}

// Unit is one node of the plan: a task for one mason and the acceptance its
// reviewer verifies. Criteria cites the spec criteria the unit serves, as
// spec#<n>. Footprint names local entity map entities by ID or alias and
// decides which units may run in parallel.
type Unit struct {
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Task        string   `json:"task"`
	Acceptance  []string `json:"acceptance"`
	Criteria    []string `json:"criteria"`
	DependsOn   []string `json:"depends_on"`
	Footprint   []string `json:"footprint"`
	Constraints []string `json:"constraints,omitempty"`
	Boundaries  []string `json:"boundaries,omitempty"`
	Guidance    []string `json:"guidance,omitempty"`
}

// Serves reports whether the unit cites criterion n.
func (u Unit) Serves(n int) bool {
	for _, c := range u.Criteria {
		if got, ok := ParseCitation(c); ok && got == n {
			return true
		}
	}
	return false
}

// Unit returns the unit with the given ID. A duplicated ID is not found.
func (p Plan) Unit(id string) (Unit, bool) {
	var found []Unit
	for _, u := range p.Units {
		if u.ID == id {
			found = append(found, u)
		}
	}
	if len(found) != 1 {
		return Unit{}, false
	}
	return found[0], true
}

// Addressing returns the units that serve criterion n, in plan order.
func (p Plan) Addressing(n int) []Unit {
	out := []Unit{}
	for _, u := range p.Units {
		if u.Serves(n) {
			out = append(out, u)
		}
	}
	return out
}

// Parse decodes plan.json. The version is checked first, so a plan of an
// unknown version is refused as ErrUnsupportedVersion whatever else it holds.
// Unknown fields and trailing data are rejected. Parse does not validate the
// plan; see Validate.
func Parse(data []byte) (Plan, error) {
	var head struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Plan{}, fmt.Errorf("plan: %w", err)
	}
	if head.Version == nil {
		return Plan{}, fmt.Errorf("plan: %w: version is missing", ErrUnsupportedVersion)
	}
	if *head.Version != Version {
		return Plan{}, fmt.Errorf("plan: %w %d", ErrUnsupportedVersion, *head.Version)
	}
	var p Plan
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("plan: %w", err)
	}
	return normalize(p), nil
}

// Encode returns the canonical form of p: units in plan order, absent lists
// written as [], two-space indentation and a trailing newline.
func Encode(p Plan) ([]byte, error) {
	if p.Version != Version {
		return nil, fmt.Errorf("plan: %w %d", ErrUnsupportedVersion, p.Version)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(normalize(p)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func normalize(p Plan) Plan {
	out := Plan{Version: p.Version, Units: make([]Unit, 0, len(p.Units))}
	for _, u := range p.Units {
		u.Constraints = slices.Clone(u.Constraints)
		u.Boundaries = slices.Clone(u.Boundaries)
		u.Guidance = slices.Clone(u.Guidance)
		u.Acceptance = append([]string{}, u.Acceptance...)
		u.Criteria = append([]string{}, u.Criteria...)
		u.DependsOn = append([]string{}, u.DependsOn...)
		u.Footprint = append([]string{}, u.Footprint...)
		out.Units = append(out.Units, u)
	}
	return out
}
