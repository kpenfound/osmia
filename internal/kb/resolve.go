package kb

import (
	"path"
	"sort"
	"strings"
)

// Footprint is the result of resolving entity names to path patterns.
type Footprint struct {
	// Entities are the IDs of the named entities and every entity that is
	// transitively part of one of them.
	Entities []string
	// Paths are their path patterns.
	Paths []string
	// Unresolved are the names that match no entity, or whose entities have
	// no path pattern at all, in input order.
	Unresolved []string
}

// PathMatch lists the most specific entities for one repository path.
type PathMatch struct {
	Path     string
	Entities []string
}

// Location is the result of resolving repository paths to entities.
type Location struct {
	Matches []PathMatch
	// Unresolved are the paths that are not clean repository-relative paths
	// or that no entity pattern matches, in input order.
	Unresolved []string
}

// Lookup finds an entity by ID or alias, ignoring case.
func (m Map) Lookup(name string) (Entity, bool) {
	key := strings.ToLower(name)
	for _, e := range m.Entities {
		if e.ID == key {
			return e, true
		}
	}
	for _, e := range m.Entities {
		for _, alias := range e.Aliases {
			if strings.ToLower(alias) == key {
				return e, true
			}
		}
	}
	return Entity{}, false
}

// ResolveEntities maps entity names to the path patterns of those entities and
// of the entities that are transitively part of them.
func (m Map) ResolveEntities(names []string) Footprint {
	children := map[string][]Entity{}
	for _, e := range m.Entities {
		for _, parent := range e.PartOf {
			children[parent] = append(children[parent], e)
		}
	}
	out := Footprint{Entities: []string{}, Paths: []string{}, Unresolved: []string{}}
	entities, paths := map[string]bool{}, map[string]bool{}
	for _, name := range names {
		e, ok := m.Lookup(name)
		if !ok {
			out.Unresolved = append(out.Unresolved, name)
			continue
		}
		found := map[string]bool{}
		var patterns []string
		var walk func(Entity)
		walk = func(e Entity) {
			if found[e.ID] {
				return
			}
			found[e.ID] = true
			patterns = append(patterns, e.Paths...)
			for _, child := range children[e.ID] {
				walk(child)
			}
		}
		walk(e)
		if len(patterns) == 0 {
			out.Unresolved = append(out.Unresolved, name)
			continue
		}
		for id := range found {
			entities[id] = true
		}
		for _, p := range patterns {
			paths[p] = true
		}
	}
	out.Entities = keys(entities)
	out.Paths = keys(paths)
	return out
}

// ResolvePaths maps repository paths to the entities whose patterns match them
// most specifically: the longest literal pattern prefix wins and ties return
// every entity that shares it.
func (m Map) ResolvePaths(paths []string) Location {
	out := Location{Matches: []PathMatch{}, Unresolved: []string{}}
	for _, p := range paths {
		if !cleanPath(p) {
			out.Unresolved = append(out.Unresolved, p)
			continue
		}
		segments := strings.Split(p, "/")
		best := -1
		var ids []string
		for _, e := range m.Entities {
			score := -1
			for _, pattern := range e.Paths {
				if n := literalPrefix(pattern); n > score && match(strings.Split(pattern, "/"), segments) {
					score = n
				}
			}
			switch {
			case score < 0:
			case score > best:
				best, ids = score, []string{e.ID}
			case score == best:
				ids = append(ids, e.ID)
			}
		}
		if len(ids) == 0 {
			out.Unresolved = append(out.Unresolved, p)
			continue
		}
		sort.Strings(ids)
		out.Matches = append(out.Matches, PathMatch{Path: p, Entities: ids})
	}
	return out
}

// match reports whether a pattern matches a path or one of its parent
// directories, so a directory pattern covers everything below it.
func match(pattern, name []string) bool {
	if len(pattern) == 0 {
		return true
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if match(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], name[0])
	return ok && err == nil && match(pattern[1:], name[1:])
}

func literalPrefix(pattern string) int {
	if i := strings.IndexAny(pattern, "*?[\\"); i >= 0 {
		return i
	}
	return len(pattern)
}

func cleanPath(p string) bool {
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\x00\r\n") {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." {
			return false
		}
	}
	return true
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
