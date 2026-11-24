package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	mapAdded   = "added"
	mapRemoved = "removed"
	mapRenamed = "renamed"
	mapMoved   = "moved"
)

type mapChange struct {
	kind                 string
	id                   string
	oldID                string
	name, oldName        string
	parents, oldParents  []string
	parentNames, oldWere []string
}

func mapUpdateRecords(in *publishInputs) []hearsay.Record {
	var revisions []trace.Document
	for _, d := range in.documents {
		if d.Path == trace.EntitiesPath {
			revisions = append(revisions, d)
		}
	}
	slices.SortStableFunc(revisions, func(a, b trace.Document) int { return a.Revision - b.Revision })
	var out []hearsay.Record
	var previous kb.Map
	have := false
	for _, d := range revisions {
		current, err := kb.Parse([]byte(d.Content))
		if err != nil {
			continue
		}
		if have {
			cause := mapCause(d)
			for _, c := range diffMap(previous, current) {
				id := mapUpdateID(d.Revision, c)
				if !hearsay.ValidID(id) {
					continue
				}
				w := hearsay.Wire{Kind: hearsay.KindUpdate, Project: string(in.project), Update: id, Text: c.text() + cause,
					CreatedAt: hearsay.Time(d.At), UpdatedAt: hearsay.Time(d.At)}
				out = append(out, hearsay.Record{Source: in.workSource, Key: "update/" + id, Tier: hearsay.TierRecord, After: []string{in.projectRef()}, Versions: []hearsay.Wire{w}})
			}
		}
		previous, have = current, true
	}
	return out
}

func mapUpdateID(revision int, c mapChange) string {
	return decisionID("map", fmt.Sprint(revision), c.kind, c.id)
}

func mapCause(d trace.Document) string {
	if commit, ok := kb.ReseedCommit(d.Cause); ok {
		return " Caused by the re-seed of the map from commit " + commit + "."
	}
	return ""
}

func (c mapChange) text() string {
	label := func(name, id string) string { return fmt.Sprintf("%q (%s)", name, id) }
	switch c.kind {
	case mapAdded:
		return "Subsystem added to the map: " + label(c.name, c.id) + c.partOf(c.parentNames) + "."
	case mapRemoved:
		return "Subsystem removed from the map: " + label(c.name, c.id) + "."
	case mapRenamed:
		return fmt.Sprintf("Subsystem renamed: %s is now %s.", label(c.oldName, c.oldID), label(c.name, c.id))
	default:
		return fmt.Sprintf("Subsystem %s moved: it was%s, and is now%s.", label(c.name, c.id), c.partOf(c.oldWere), c.partOf(c.parentNames))
	}
}

func (c mapChange) partOf(parents []string) string {
	if len(parents) == 0 {
		return " at the top level"
	}
	return " part of " + strings.Join(parents, ", ")
}

func diffMap(before, after kb.Map) []mapChange {
	old := map[string]kb.Entity{}
	for _, e := range before.Entities {
		old[e.ID] = e
	}
	current := map[string]kb.Entity{}
	for _, e := range after.Entities {
		current[e.ID] = e
	}
	var gone, fresh []string
	for id := range old {
		if _, ok := current[id]; !ok {
			gone = append(gone, id)
		}
	}
	for id := range current {
		if _, ok := old[id]; !ok {
			fresh = append(fresh, id)
		}
	}
	slices.Sort(gone)
	slices.Sort(fresh)

	renamedTo := map[string]string{}
	taken := map[string]bool{}
	for _, was := range gone {
		path := old[was].Primary()
		if path == "" {
			continue
		}
		for _, now := range fresh {
			if !taken[now] && current[now].Primary() == path {
				renamedTo[was], taken[now] = now, true
				break
			}
		}
	}
	names := func(m map[string]kb.Entity, ids []string) []string {
		var out []string
		for _, id := range ids {
			if e, ok := m[id]; ok {
				out = append(out, e.Name)
			} else {
				out = append(out, id)
			}
		}
		slices.Sort(out)
		return out
	}
	parentsOf := func(e kb.Entity) []string {
		out := slices.Clone(e.PartOf)
		slices.Sort(out)
		return out
	}
	mapped := func(ids []string) []string {
		out := make([]string, len(ids))
		for i, id := range ids {
			if now, ok := renamedTo[id]; ok {
				id = now
			}
			out[i] = id
		}
		slices.Sort(out)
		return out
	}
	moved := func(was, now kb.Entity) *mapChange {
		oldParents, parents := parentsOf(was), parentsOf(now)
		if slices.Equal(mapped(oldParents), parents) {
			return nil
		}
		return &mapChange{kind: mapMoved, id: now.ID, name: now.Name, parents: parents, oldParents: oldParents,
			parentNames: names(current, parents), oldWere: names(old, oldParents)}
	}

	var out []mapChange
	for _, id := range gone {
		if now, ok := renamedTo[id]; ok {
			out = append(out, mapChange{kind: mapRenamed, id: now, oldID: id, name: current[now].Name, oldName: old[id].Name})
			if c := moved(old[id], current[now]); c != nil {
				out = append(out, *c)
			}
			continue
		}
		out = append(out, mapChange{kind: mapRemoved, id: id, name: old[id].Name})
	}
	for _, id := range fresh {
		if !taken[id] {
			parents := parentsOf(current[id])
			out = append(out, mapChange{kind: mapAdded, id: id, name: current[id].Name, parents: parents, parentNames: names(current, parents)})
		}
	}
	kept := make([]string, 0, len(current))
	for id := range current {
		if _, ok := old[id]; ok {
			kept = append(kept, id)
		}
	}
	slices.Sort(kept)
	for _, id := range kept {
		was, now := old[id], current[id]
		if was.Name != now.Name {
			out = append(out, mapChange{kind: mapRenamed, id: id, oldID: id, name: now.Name, oldName: was.Name})
		}
		if c := moved(was, now); c != nil {
			out = append(out, *c)
		}
	}
	return out
}
