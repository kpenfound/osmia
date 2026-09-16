package plan

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/kb"
)

// Kind classifies a validation problem.
type Kind string

const (
	// SpecProblem is a diagnostic from the spec's criteria list.
	SpecProblem Kind = "spec"
	// UnitProblem is a malformed or duplicate unit ID, or a criterion a unit
	// addresses more than once.
	UnitProblem Kind = "unit"
	// UnknownDependency is a dependency on a unit the plan does not have.
	UnknownDependency Kind = "unknown-dependency"
	// DependencyCycle is a cycle in the dependencies.
	DependencyCycle Kind = "dependency-cycle"
	// UncoveredCriterion is a spec criterion no unit addresses.
	UncoveredCriterion Kind = "uncovered-criterion"
	// UnknownCriterion is an addressed criterion the spec does not have.
	UnknownCriterion Kind = "unknown-criterion"
	// MissingProof is an addressed criterion without a named proof.
	MissingProof Kind = "missing-proof"
	// UnresolvedFootprint is a footprint that does not resolve against the
	// entity map.
	UnresolvedFootprint Kind = "unresolved-footprint"
)

// Problem is one validation failure. Unit or Criterion, or both, name what is
// at fault; Criterion is a spec#<n> citation as the plan wrote it. A spec
// problem that concerns no single criterion names neither.
type Problem struct {
	Kind      Kind   `json:"kind"`
	Unit      string `json:"unit,omitempty"`
	Criterion string `json:"criterion,omitempty"`
	Message   string `json:"message"`
}

func (p Problem) Error() string {
	var at []string
	if p.Unit != "" {
		at = append(at, fmt.Sprintf("unit %q", p.Unit))
	}
	if p.Criterion != "" {
		at = append(at, p.Criterion)
	}
	if len(at) == 0 {
		return p.Message
	}
	return strings.Join(at, " ") + ": " + p.Message
}

var unitID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Validate returns every problem with a plan against its spec and the local
// entity map, or an empty list when the plan can be presented. It does not
// judge unit size.
func Validate(spec Spec, p Plan, entities kb.Map) []Problem {
	out := []Problem{}
	add := func(kind Kind, unit, criterion, format string, args ...any) {
		out = append(out, Problem{kind, unit, criterion, fmt.Sprintf(format, args...)})
	}
	for _, d := range spec.Diagnostics {
		criterion := ""
		if d.Criterion > 0 {
			criterion = Cite(d.Criterion)
		}
		if d.Line > 0 {
			add(SpecProblem, "", criterion, "%s line %d: %s", SpecPath, d.Line, d.Message)
		} else {
			add(SpecProblem, "", criterion, "%s: %s", SpecPath, d.Message)
		}
	}

	units := map[string]bool{}
	for _, u := range p.Units {
		switch {
		case !unitID.MatchString(u.ID):
			add(UnitProblem, u.ID, "", "id must be 1-128 letters, digits, '_' or '-', starting with a letter or digit")
		case units[u.ID]:
			add(UnitProblem, u.ID, "", "duplicate unit id")
		}
		units[u.ID] = true
	}

	covered := map[int]bool{}
	for _, u := range p.Units {
		for _, dep := range u.DependsOn {
			if !units[dep] {
				add(UnknownDependency, u.ID, "", "depends on unit %q, which the plan does not have", dep)
			}
		}
		seen := map[string]bool{}
		for _, a := range u.Addresses {
			n, ok := ParseCitation(a.Criterion)
			switch {
			case !ok:
				add(UnknownCriterion, u.ID, a.Criterion, "is not a spec#<n> citation")
			case len(spec.numbered(n)) == 0:
				add(UnknownCriterion, u.ID, a.Criterion, "the spec has no criterion %d", n)
			case len(spec.numbered(n)) > 1:
				add(UnknownCriterion, u.ID, a.Criterion, "the spec numbers criterion %d more than once, so it cannot be cited", n)
			default:
				covered[n] = true
			}
			if seen[a.Criterion] {
				add(UnitProblem, u.ID, a.Criterion, "addressed more than once")
			}
			seen[a.Criterion] = true
			switch {
			case strings.TrimSpace(string(a.Proof.Kind)) == "" && strings.TrimSpace(a.Proof.Name) == "":
				add(MissingProof, u.ID, a.Criterion, "no proof is named")
			case !a.Proof.Kind.Valid():
				add(MissingProof, u.ID, a.Criterion, "proof kind %q is not one of %s, %s, %s or %s", a.Proof.Kind, NewTest, ExistingTest, ScriptedCheck, ReviewerJudgement)
			case strings.TrimSpace(a.Proof.Name) == "":
				add(MissingProof, u.ID, a.Criterion, "the %s proof has no name", a.Proof.Kind)
			}
		}
		if len(u.Footprint) == 0 {
			add(UnresolvedFootprint, u.ID, "", "declares no footprint")
		}
		for _, name := range entities.ResolveEntities(u.Footprint).Unresolved {
			add(UnresolvedFootprint, u.ID, "", "footprint %q names no entity with a path in kb/entities.json", name)
		}
	}

	for _, n := range spec.numbers() {
		if !covered[n] {
			add(UncoveredCriterion, "", Cite(n), "no unit addresses this criterion")
		}
	}

	for _, cycle := range cycles(p) {
		add(DependencyCycle, cycle[0], "", "dependency cycle %s", strings.Join(cycle, " -> "))
	}
	return out
}

// numbered returns the criteria numbered n.
func (s Spec) numbered(n int) []Criterion {
	var out []Criterion
	for _, c := range s.Criteria {
		if c.Number == n {
			out = append(out, c)
		}
	}
	return out
}

// numbers returns the distinct criterion numbers in ascending order.
func (s Spec) numbers() []int {
	var out []int
	for _, c := range s.Criteria {
		if !slices.Contains(out, c.Number) {
			out = append(out, c.Number)
		}
	}
	slices.Sort(out)
	return out
}

// cycles returns the dependency cycles a depth-first walk finds, at least one
// in every cyclic group, each starting at its smallest unit ID.
func cycles(p Plan) [][]string {
	edges := map[string][]string{}
	var order []string
	for _, u := range p.Units {
		if _, ok := edges[u.ID]; !ok {
			edges[u.ID] = u.DependsOn
			order = append(order, u.ID)
		}
	}
	var found [][]string
	seen, done := map[string]bool{}, map[string]bool{}
	var stack []string
	var visit func(string)
	visit = func(id string) {
		if i := slices.Index(stack, id); i >= 0 {
			cycle := slices.Clone(stack[i:])
			start := slices.Index(cycle, slices.Min(cycle))
			cycle = append(cycle[start:], cycle[:start]...)
			cycle = append(cycle, cycle[0])
			key := strings.Join(cycle, "\x00")
			if !seen[key] {
				seen[key] = true
				found = append(found, cycle)
			}
			return
		}
		if done[id] {
			return
		}
		stack = append(stack, id)
		for _, next := range edges[id] {
			if _, ok := edges[next]; ok {
				visit(next)
			}
		}
		stack = stack[:len(stack)-1]
		done[id] = true
	}
	for _, id := range order {
		visit(id)
	}
	slices.SortFunc(found, func(a, b []string) int { return strings.Compare(strings.Join(a, "\x00"), strings.Join(b, "\x00")) })
	return found
}
