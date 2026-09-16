package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the only plan.json schema version.
const Version = 1

// ErrUnsupportedVersion reports a plan.json whose version is missing or not
// Version.
var ErrUnsupportedVersion = errors.New("unsupported plan version")

// Plan is the content of plan.json: the directed graph of units.
type Plan struct {
	Version int    `json:"version"`
	Units   []Unit `json:"units"`
}

// Unit is one node of the plan. Footprint names local entity map entities by
// ID or alias.
type Unit struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Addresses []Address `json:"addresses"`
	DependsOn []string  `json:"depends_on"`
	Footprint []string  `json:"footprint"`
}

// Address names a criterion, as spec#<n>, and how the unit will show it holds.
type Address struct {
	Criterion string `json:"criterion"`
	Proof     Proof  `json:"proof"`
}

// ProofKind is one of the supported forms of proof.
type ProofKind string

const (
	NewTest           ProofKind = "new-test"
	ExistingTest      ProofKind = "existing-test"
	ScriptedCheck     ProofKind = "scripted-check"
	ReviewerJudgement ProofKind = "reviewer-judgement"
)

// Valid reports whether k is a supported proof kind.
func (k ProofKind) Valid() bool {
	switch k {
	case NewTest, ExistingTest, ScriptedCheck, ReviewerJudgement:
		return true
	}
	return false
}

// Proof names the evidence for one criterion: the test, the check or what the
// reviewer will judge.
type Proof struct {
	Kind ProofKind `json:"kind"`
	Name string    `json:"name"`
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

// Addressing returns the units that address criterion n, in plan order.
func (p Plan) Addressing(n int) []Unit {
	out := []Unit{}
	for _, u := range p.Units {
		for _, a := range u.Addresses {
			if got, ok := ParseCitation(a.Criterion); ok && got == n {
				out = append(out, u)
				break
			}
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
		u.Addresses = append([]Address{}, u.Addresses...)
		u.DependsOn = append([]string{}, u.DependsOn...)
		u.Footprint = append([]string{}, u.Footprint...)
		out.Units = append(out.Units, u)
	}
	return out
}
