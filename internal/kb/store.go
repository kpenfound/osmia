package kb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// Merge regenerates a map from a fresh seed without disturbing existing
// entities. An existing entity whose primary path matches a seeded one is kept
// as it is, with its ID; seeded entities that are new are added, their part_of
// edges pointing at the kept entities, and any alias that would collide is
// dropped. Existing entities the seed no longer produces are kept.
func Merge(existing, seed Map) Map {
	out := Map{Version: Version, Entities: []Entity{}}
	byPath := map[string]string{}
	names := map[string]bool{}
	for _, e := range existing.Entities {
		out.Entities = append(out.Entities, e)
		if p := e.Primary(); p != "" {
			if _, ok := byPath[p]; !ok {
				byPath[p] = e.ID
			}
		}
		names[strings.ToLower(e.ID)] = true
		for _, a := range e.Aliases {
			names[strings.ToLower(a)] = true
		}
	}
	// renamed maps seed IDs to the IDs they take in the merged map.
	renamed := map[string]string{}
	var added []Entity
	for _, e := range seed.Entities {
		if id, ok := byPath[e.Primary()]; ok {
			renamed[e.ID] = id
			continue
		}
		id := e.ID
		for n := 2; names[id]; n++ {
			id = fmt.Sprintf("%s-%d", e.ID, n)
		}
		names[id] = true
		renamed[e.ID] = id
		e.ID = id
		added = append(added, e)
	}
	for _, e := range added {
		aliases := []string{}
		for _, a := range e.Aliases {
			if !names[strings.ToLower(a)] {
				names[strings.ToLower(a)] = true
				aliases = append(aliases, a)
			}
		}
		parents := []string{}
		for _, p := range e.PartOf {
			parents = append(parents, renamed[p])
		}
		e.Aliases, e.PartOf = aliases, parents
		out.Entities = append(out.Entities, e)
	}
	return out
}

// Load returns the latest entity map revision recorded in the trace, or an
// empty map when none is recorded.
func Load(r *trace.Repository) (Map, error) {
	m, _, err := LoadRevision(r)
	return m, err
}

// LoadRevision is Load that also returns the revision it read, 0 when none is
// recorded.
func LoadRevision(r *trace.Repository) (Map, int, error) {
	content, revision, err := latest(r)
	if err != nil {
		return Map{}, 0, err
	}
	m, err := Parse([]byte(content))
	return m, revision, err
}

// Store validates m and records it as the next Document revision of
// kb/entities.json. Nothing is written when m is invalid.
func Store(ctx context.Context, r *trace.Repository, m Map, at time.Time, actor trace.Actor, cause string) error {
	data, err := Encode(m)
	if err != nil {
		return err
	}
	_, revision, err := latest(r)
	if err != nil {
		return err
	}
	return r.Append(ctx, trace.Document{
		Header:  trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.EntitiesDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    trace.EntitiesPath,
		Content: string(data),
	})
}

func latest(r *trace.Repository) (string, int, error) {
	documents, err := trace.Read[trace.Document](r, "")
	if err != nil {
		return "", 0, err
	}
	content, revision := "", 0
	for _, d := range documents {
		if d.Path == trace.EntitiesPath && d.Revision > revision {
			content, revision = d.Content, d.Revision
		}
	}
	return content, revision, nil
}
