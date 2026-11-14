package service

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// The structural changes of the subsystem map that are posted.
const (
	mapAdded   = "added"
	mapRemoved = "removed"
	mapRenamed = "renamed"
	mapMoved   = "moved"
)

// mapChange is one structural change between two revisions of the subsystem
// map: a subsystem added, removed, renamed, or moved under a different
// parent. Notes, aliases, paths and owners are not structure.
type mapChange struct {
	kind string
	// id is the subsystem's id after the change, or before it for a removal.
	id string
	// oldID differs from id for a rename that changed the id.
	oldID                string
	name, oldName        string
	parents, oldParents  []string
	parentNames, oldWere []string
}

// librarianOperations reads the librarian's recorded operations by ID. Each
// revision of the entity map names the operation that recorded it. A
// librarian trace that cannot be read leaves the map without attributions.
func librarianOperations(repo *trace.Repository) map[string]coreadapter.Operation {
	stream := librarianWorkstream(repo.Project())
	streams, err := repo.Workstreams()
	if err != nil || !slices.Contains(streams, stream) {
		return nil
	}
	records, err := repo.Operations(stream)
	if err != nil {
		return nil
	}
	out := make(map[string]coreadapter.Operation, len(records))
	for _, r := range records {
		out[r.Operation.ID] = r.Operation
	}
	return out
}

// mapUpdateRecords posts the structural changes between consecutive committed
// revisions of kb/entities.json as dated updates on the project, on the work
// source and without an author. Each names the workstream whose landed work
// caused the revision, or the extraction when no workstream did. Only the
// map's structure is posted: subsystem notes, the source ledger and entity
// owners are not.
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
			cause := in.mapCause(d)
			for _, c := range diffMap(previous, current) {
				id := mapUpdateID(d.Revision, c)
				if !hearsay.ValidID(id) {
					continue
				}
				w := hearsay.Wire{Kind: hearsay.KindUpdate, Project: string(in.project), Update: id, Text: c.text() + " " + cause,
					CreatedAt: hearsay.Time(d.At), UpdatedAt: hearsay.Time(d.At)}
				out = append(out, hearsay.Record{Source: in.workSource, Key: "update/" + id, Tier: hearsay.TierRecord, After: []string{in.projectRef()}, Versions: []hearsay.Wire{w}})
			}
		}
		previous, have = current, true
	}
	return out
}

// mapUpdateID is the id of the update that reports one change of one revision
// of the map.
func mapUpdateID(revision int, c mapChange) string {
	return decisionID("map", fmt.Sprint(revision), c.kind, c.id)
}

// mapCause names what caused a revision of the map: the workstream a
// knowledge refresh folded in, or else the extraction.
func (in *publishInputs) mapCause(d trace.Document) string {
	op, ok := in.operations[d.Cause]
	if !ok {
		return "Caused by a knowledge-base extraction."
	}
	switch op.Action {
	case RefreshAction:
		var input refreshInput
		if json.Unmarshal(op.Input, &input) == nil && input.Workstream != "" {
			return "Caused by workstream " + in.workstreamLabel(input.Workstream) + "."
		}
	case ExtractAction:
		var input extractionInput
		if json.Unmarshal(op.Input, &input) == nil && input.Extraction > 0 {
			return fmt.Sprintf("Caused by knowledge-base extraction %d.", input.Extraction)
		}
	}
	return "Caused by a knowledge-base extraction."
}

// workstreamLabel is a workstream's id with its title, when it is still
// posted.
func (in *publishInputs) workstreamLabel(id config.WorkstreamID) string {
	for _, st := range in.streams {
		if st.id() == id {
			return fmt.Sprintf("%s (%s)", id, st.title())
		}
	}
	return string(id)
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

// diffMap lists the structural changes from one revision of the map to the
// next, in a stable order. A subsystem whose id is gone and whose primary
// path now belongs to a new id is a rename, as is one that kept its id and
// changed its name. Parents are compared after following renames, so
// renaming a parent does not move its children.
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
	// mapped follows the renames of the old revision's parents.
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
