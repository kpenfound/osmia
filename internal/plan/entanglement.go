package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/kb"
)

// BlockReason identifies why an in-flight unit prevents a ready unit starting.
type BlockReason string

const (
	DependencyReason BlockReason = "dependency"
	OverlapReason    BlockReason = "footprint-overlap"
	MappingReason    BlockReason = "unresolved-footprint"
)

// StartBlocker names one in-flight unit and the reason it blocks a candidate.
type StartBlocker struct {
	Unit   string      `json:"unit"`
	Reason BlockReason `json:"reason"`
}

// StartDecision is the pure eligibility result for a ready unit.
type StartDecision struct {
	CanStart bool           `json:"can_start"`
	Blockers []StartBlocker `json:"blockers"`
}

// DecideStart compares a ready unit with the units in flight in its sealed
// plan. The caller supplies the local entity map used for this decision.
// Blockers are sorted by unit ID. Mapping uncertainty takes precedence over a
// dependency, which takes precedence over a resolved footprint overlap.
func DecideStart(p Plan, entities kb.Map, candidate string, inFlight []string) (StartDecision, error) {
	units := make(map[string]Unit, len(p.Units))
	for _, u := range p.Units {
		units[u.ID] = u
	}
	c, ok := units[candidate]
	if !ok {
		return StartDecision{}, fmt.Errorf("candidate unit %q is not in the plan", candidate)
	}
	ids := append([]string{}, inFlight...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if id == candidate {
			return StartDecision{}, fmt.Errorf("candidate unit %q is already in flight", candidate)
		}
		if _, ok := units[id]; !ok {
			return StartDecision{}, fmt.Errorf("in-flight unit %q is not in the plan", id)
		}
	}
	cPaths, cUnknown := resolvedPaths(entities, c.Footprint)
	result := StartDecision{CanStart: true, Blockers: []StartBlocker{}}
	for _, id := range ids {
		u := units[id]
		uPaths, uUnknown := resolvedPaths(entities, u.Footprint)
		var reason BlockReason
		switch {
		case cUnknown || uUnknown:
			reason = MappingReason
		case dependsOn(units, candidate, id) || dependsOn(units, id, candidate):
			reason = DependencyReason
		case pathsOverlap(cPaths, uPaths):
			reason = OverlapReason
		}
		if reason != "" {
			result.Blockers = append(result.Blockers, StartBlocker{Unit: id, Reason: reason})
		}
	}
	result.CanStart = len(result.Blockers) == 0
	return result, nil
}

func resolvedPaths(m kb.Map, names []string) ([]string, bool) {
	if len(names) == 0 {
		return nil, true
	}
	// A malformed or changing map can contain colliding names. Resolution's
	// first-match lookup alone must not certify either interpretation as safe.
	for _, name := range names {
		matches := map[string]bool{}
		for _, e := range m.Entities {
			if strings.EqualFold(e.ID, name) {
				matches[e.ID] = true
			}
			for _, alias := range e.Aliases {
				if strings.EqualFold(alias, name) {
					matches[e.ID] = true
				}
			}
		}
		if len(matches) != 1 {
			return nil, true
		}
	}
	resolved := m.ResolveEntities(names)
	return resolved.Paths, len(resolved.Unresolved) != 0 || len(resolved.Paths) == 0
}

func dependsOn(units map[string]Unit, from, target string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, dep := range units[id].DependsOn {
			if dep == target || visit(dep) {
				return true
			}
		}
		return false
	}
	return visit(from)
}

func pathsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if kb.PatternsOverlap(x, y) {
				return true
			}
		}
	}
	return false
}
