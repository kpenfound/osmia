package amendment

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	Leave    = "unchanged"
	Replan   = "replan"
	Rework   = "rework"
	FollowUp = "follow-up"
	Cancel   = "cancel"
	Revise   = "revision"
	Remove   = "obsolete"
	Add      = "new"
)

// Member captures concrete workflow evidence used to classify a proposed delta.
// Merged means the workstream's pull request merged, rather than a unit landing.
type Member struct {
	Workstream  config.WorkstreamID `json:"workstream"`
	State       string              `json:"state"`
	Merged      bool                `json:"merged"`
	PullRequest int                 `json:"pull_request,omitempty"`
	Units       map[string]string   `json:"units"`
}

type UnitImpact struct {
	Unit     string `json:"unit"`
	Action   string `json:"action"`
	Revision string `json:"revision,omitempty"`
}

type StreamImpact struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Action     string              `json:"action"`
	Pause      bool                `json:"pause"`
	Cascade    bool                `json:"cascade"`
	Units      []UnitImpact        `json:"units"`
}

type Impact struct {
	Streams   []StreamImpact `json:"streams"`
	Contracts []string       `json:"contracts"`
}

// ChangedCriteria compares approved and proposed text. Models cannot declare
// which criteria changed. A prose-only change conservatively affects the slice.
func ChangedCriteria(before, after string) []string {
	old, now := map[int]string{}, map[int]string{}
	for _, c := range plan.ParseSpec(before).Criteria {
		old[c.Number] = c.Text
	}
	for _, c := range plan.ParseSpec(after).Criteria {
		now[c.Number] = c.Text
	}
	out := []string{}
	for n, text := range old {
		if now[n] != text {
			out = append(out, plan.Cite(n))
		}
	}
	for n, text := range now {
		if _, ok := old[n]; !ok && text != "" {
			out = append(out, plan.Cite(n))
		}
	}
	if before != after && len(out) == 0 {
		for n := range old {
			out = append(out, plan.Cite(n))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func changedContracts(before, after trace.ProjectSpec) []string {
	old, now := map[string]string{}, map[string]string{}
	for _, c := range before.Contracts {
		old[c.Name] = c.Text
	}
	for _, c := range after.Contracts {
		now[c.Name] = c.Text
	}
	out := []string{}
	for n, t := range old {
		if now[n] != t {
			out = append(out, n)
		}
	}
	for n, t := range now {
		if old[n] != t {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ClassifyImpact includes unchanged units and streams explicitly, preserving
// their evidence. Contract changes re-evaluate the transitive edge closure;
// only each dependent's contract criteria are reworked in an unchanged slice.
func ClassifyImpact(id string, before, after trace.ProjectSpec, members []Member) (Impact, error) {
	out := Impact{Contracts: changedContracts(before, after)}
	ids := make([]config.WorkstreamID, 0, len(members))
	affected := map[config.WorkstreamID]bool{}
	for _, m := range members {
		ids = append(ids, m.Workstream)
		a, aok := trace.SliceFor(before, m.Workstream)
		b, bok := trace.SliceFor(after, m.Workstream)
		if !aok || !bok {
			return out, fmt.Errorf("amendment must cover member %s", m.Workstream)
		}
		affected[m.Workstream] = !reflect.DeepEqual(a, b)
	}
	if len(after.Slices) != len(members) {
		return out, fmt.Errorf("amendment must cover exactly the workgroup's members")
	}
	if err := trace.ValidateProjectSpec(after, ids); err != nil {
		return out, err
	}
	cascade := map[config.WorkstreamID]bool{}
	if len(out.Contracts) > 0 {
		// A changed contract without a changed slice still invalidates all
		// criteria that the slices explicitly mark as that shared interface.
		for _, sl := range before.Slices {
			if len(sl.ContractCriteria) > 0 {
				affected[sl.Workstream] = true
				cascade[sl.Workstream] = true
			}
		}
		for _, sl := range after.Slices {
			if len(sl.ContractCriteria) > 0 {
				affected[sl.Workstream] = true
				cascade[sl.Workstream] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for _, e := range append(slices.Clone(before.Graph), after.Graph...) {
				if affected[e.From] && !affected[e.To] {
					affected[e.To], cascade[e.To], changed = true, true, true
				}
			}
		}
	}
	for _, m := range members {
		a, _ := trace.SliceFor(before, m.Workstream)
		b, _ := trace.SliceFor(after, m.Workstream)
		old, err := plan.Parse([]byte(a.Plan))
		if err != nil {
			return out, err
		}
		now, err := plan.Parse([]byte(b.Plan))
		if err != nil {
			return out, err
		}
		criteria := ChangedCriteria(a.Spec, b.Spec)
		if cascade[m.Workstream] {
			for _, n := range append(slices.Clone(a.ContractCriteria), b.ContractCriteria...) {
				criteria = append(criteria, plan.Cite(n))
			}
		}
		row := StreamImpact{Workstream: m.Workstream, Action: Leave, Cascade: cascade[m.Workstream]}
		seen := map[string]bool{}
		for _, u := range old.Units {
			seen[u.ID] = true
			next, exists := now.Unit(u.ID)
			action := Leave
			if !exists {
				action = Remove
			} else if !reflect.DeepEqual(u, next) || slices.ContainsFunc(append(slices.Clone(u.Criteria), next.Criteria...), func(c string) bool { return slices.Contains(criteria, c) }) {
				action = Rework
			}
			unit := UnitImpact{Unit: u.ID, Action: action}
			if action == Remove || action == Rework && m.Units[u.ID] == "merged" {
				unit.Revision = RevisionID(id, u.ID)
				if action == Rework {
					unit.Action = Revise
				}
			}
			row.Units = append(row.Units, unit)
		}
		for _, u := range now.Units {
			if !seen[u.ID] {
				row.Units = append(row.Units, UnitImpact{Unit: u.ID, Action: Add})
			}
		}
		touched := affected[m.Workstream] || slices.ContainsFunc(row.Units, func(u UnitImpact) bool { return u.Action != Leave })
		if touched {
			switch {
			case m.Merged:
				row.Action = FollowUp
			case m.PullRequest > 0:
				row.Action = Rework
				row.Pause = true
			case m.State == "building" || m.State == "assembled":
				row.Action = Rework
				row.Pause = true
			default:
				row.Action = Replan
			}
		}
		out.Streams = append(out.Streams, row)
	}
	return out, nil
}

func RevisionID(amendment, unit string) string {
	sum := sha256.Sum256([]byte(amendment + "\x00" + unit))
	return fmt.Sprintf("revision-%.38s-%x", unit, sum[:8])
}

// CancelImpact cancels the requested members and all transitive dependents.
// Merged work remains intact, including merged dependents in that closure.
func CancelImpact(target []config.WorkstreamID, graph []trace.Edge, members []Member) Impact {
	selected := map[config.WorkstreamID]bool{}
	for _, id := range target {
		selected[id] = true
	}
	for changed := true; changed; {
		changed = false
		for _, e := range graph {
			if selected[e.From] && !selected[e.To] {
				selected[e.To], changed = true, true
			}
		}
	}
	out := Impact{Contracts: []string{}}
	for _, m := range members {
		row := StreamImpact{Workstream: m.Workstream, Action: Leave, Units: []UnitImpact{}}
		if selected[m.Workstream] && !m.Merged && m.State != "abandoned" {
			row.Action = Cancel
			row.Pause = true
			row.Cascade = !slices.Contains(target, m.Workstream)
		}
		out.Streams = append(out.Streams, row)
	}
	return out
}

// RevisionPlan preserves completed units and routes dependencies through their
// revisions. Removal units retain the footprint needed to remove landed code.
func RevisionPlan(id string, before, after plan.Plan, impact StreamImpact) plan.Plan {
	out := plan.Plan{Version: plan.Version}
	revisions := map[string]string{}
	for _, i := range impact.Units {
		if i.Revision != "" {
			revisions[i.Unit] = i.Revision
		}
	}
	for _, i := range impact.Units {
		old, _ := before.Unit(i.Unit)
		now, exists := after.Unit(i.Unit)
		switch i.Action {
		case Leave:
			out.Units = append(out.Units, old)
		case Remove:
			now = old
			now.ID = i.Revision
			now.Title = "Remove obsolete code"
			now.Task = "Remove the code for the removed spec section implemented by " + old.ID + ". " + old.Task
			now.Acceptance = []string{"Code for the removed section is absent; the remaining spec still holds"}
			now.Criteria = nil
			out.Units = append(out.Units, now)
		case Revise:
			out.Units = append(out.Units, old)
			now.ID = i.Revision
			now.Title = "Revise " + now.Title
			now.Task = "Bring the code implemented by " + old.ID + " into line with the amended slice. " + now.Task
			now.DependsOn = append(slices.Clone(now.DependsOn), old.ID)
			out.Units = append(out.Units, now)
		default:
			if exists {
				out.Units = append(out.Units, now)
			}
		}
	}
	for n := range out.Units {
		u := &out.Units[n]
		// Historical completed units retain their original assignments.
		original := slices.ContainsFunc(impact.Units, func(i UnitImpact) bool { return i.Unit == u.ID && (i.Action == Leave || i.Action == Revise) })
		if original {
			u.DependsOn = slices.Clone(u.DependsOn)
			for j, d := range u.DependsOn {
				for _, i := range impact.Units {
					if i.Unit == d && i.Action == Remove {
						u.DependsOn[j] = i.Revision
					}
				}
			}
			continue
		}
		u.DependsOn = slices.Clone(u.DependsOn)
		for j, d := range u.DependsOn {
			if r := revisions[d]; r != "" && r != u.ID {
				u.DependsOn[j] = r
			}
		}
		slices.Sort(u.DependsOn)
		u.DependsOn = slices.Compact(u.DependsOn)
	}
	return out
}

func (i Impact) Summary() string {
	var b strings.Builder
	b.WriteString("Impact summary\n\n| Workstream | Action | Pause at unit boundary | Units |\n| --- | --- | --- | --- |\n")
	for _, row := range i.Streams {
		units := []string{}
		for _, u := range row.Units {
			if u.Action != Leave {
				units = append(units, u.Unit+": "+u.Action)
			}
		}
		action := row.Action
		if row.Cascade {
			action += " (dependent)"
		}
		fmt.Fprintf(&b, "| %s | %s | %t | %s |\n", row.Workstream, action, row.Pause, strings.Join(units, ", "))
	}
	b.WriteString("\nMerged work stays intact. If a PR merges before its change boundary, its changed sections become follow-up work in the same workgroup.\n")
	return b.String()
}
