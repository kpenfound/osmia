// Package kb holds the local knowledge base records Osmia keeps in a project
// trace: the typed entity map in kb/entities.json, its deterministic seed from
// a target clone, and footprint resolution that needs nothing but that map.
package kb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Version is the only entity map schema version.
const Version = 1

// Map is the content of kb/entities.json.
type Map struct {
	Version  int      `json:"version"`
	Entities []Entity `json:"entities"`
}

// Entity names a part of the code. Paths are repository-relative glob
// patterns; the first one is the entity's primary path. PartOf lists the IDs of
// the entities this one belongs to.
type Entity struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Paths   []string `json:"paths"`
	Owners  []string `json:"owners"`
	PartOf  []string `json:"part_of"`
}

// Primary returns the entity's primary path pattern, or "" when it has none.
func (e Entity) Primary() string {
	if len(e.Paths) == 0 {
		return ""
	}
	return e.Paths[0]
}

// Problem is one validation failure, naming the entity at fault.
type Problem struct {
	Entity  string
	Message string
}

func (p Problem) Error() string { return fmt.Sprintf("entity %q: %s", p.Entity, p.Message) }

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
var unsafeID = regexp.MustCompile(`[^a-z0-9.-]`)

// ID derives an entity ID from a primary path: lowercase, "/" becomes ".", and
// every other character outside [a-z0-9.-] becomes "-".
func ID(primary string) string {
	return unsafeID.ReplaceAllString(strings.ReplaceAll(strings.ToLower(primary), "/", "."), "-")
}

// Validate returns every problem in m joined into one error, or nil.
func Validate(m Map) error {
	var problems []error
	add := func(entity, format string, args ...any) {
		problems = append(problems, Problem{entity, fmt.Sprintf(format, args...)})
	}
	if m.Version != Version {
		problems = append(problems, fmt.Errorf("unsupported entity map version %d", m.Version))
	}
	ids := map[string]int{}
	for i, e := range m.Entities {
		if !idPattern.MatchString(e.ID) {
			add(e.ID, "id must be non-empty lowercase letters, digits, dots and hyphens")
		}
		if _, ok := ids[e.ID]; ok {
			add(e.ID, "duplicate id")
			continue
		}
		ids[e.ID] = i
	}
	// names maps every lowercased ID and alias to the entity that holds it.
	names := map[string]string{}
	for _, e := range m.Entities {
		names[strings.ToLower(e.ID)] = e.ID
	}
	for _, e := range m.Entities {
		if strings.TrimSpace(e.Name) == "" || strings.ContainsAny(e.Name, "\x00\r\n") {
			add(e.ID, "name is required and must be one line")
		}
		for _, alias := range e.Aliases {
			if strings.TrimSpace(alias) == "" || strings.ContainsAny(alias, "\x00\r\n") {
				add(e.ID, "alias %q must be non-empty and one line", alias)
				continue
			}
			key := strings.ToLower(alias)
			if holder, ok := names[key]; ok && holder != e.ID {
				add(e.ID, "alias %q collides with an id or alias of entity %q", alias, holder)
				continue
			}
			names[key] = e.ID
		}
		for _, p := range e.Paths {
			if err := CheckPattern(p); err != nil {
				add(e.ID, "%v", err)
			}
		}
		for _, owner := range e.Owners {
			if strings.TrimSpace(owner) == "" || strings.ContainsAny(owner, "\x00\r\n") {
				add(e.ID, "owner %q must be non-empty and one line", owner)
			}
		}
		for _, parent := range e.PartOf {
			if _, ok := ids[parent]; !ok {
				add(e.ID, "part_of names unknown entity %q", parent)
			}
		}
	}
	for _, cycle := range cycles(m) {
		add(cycle[0], "part_of cycle %s", strings.Join(cycle, " -> "))
	}
	return errors.Join(problems...)
}

// cycles returns the part_of cycles a depth-first walk finds, at least one in
// every cyclic group, each starting at its smallest ID.
func cycles(m Map) [][]string {
	edges := map[string][]string{}
	for _, e := range m.Entities {
		if _, ok := edges[e.ID]; !ok {
			edges[e.ID] = e.PartOf
		}
	}
	var found [][]string
	seen, done := map[string]bool{}, map[string]bool{}
	for _, e := range m.Entities {
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
		visit(e.ID)
	}
	sort.Slice(found, func(i, j int) bool { return strings.Join(found[i], "\x00") < strings.Join(found[j], "\x00") })
	return found
}

// CheckPattern accepts a repository-relative glob pattern: slash-separated
// segments, each a path.Match pattern or "**" for any number of segments. It
// rejects empty, absolute and unclean patterns and any "." or ".." segment.
func CheckPattern(p string) error {
	if p == "" || strings.ContainsAny(p, "\x00\r\n") {
		return fmt.Errorf("path pattern %q must be non-empty and one line", p)
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path pattern %q is absolute", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." {
			return fmt.Errorf("path pattern %q escapes the repository", p)
		}
		if s == "" || s == "." {
			return fmt.Errorf("path pattern %q has an empty or \".\" segment", p)
		}
		if s != "**" && strings.Contains(s, "**") {
			return fmt.Errorf("path pattern %q uses ** inside a segment", p)
		}
		if _, err := path.Match(s, ""); err != nil {
			return fmt.Errorf("path pattern %q is not a valid glob", p)
		}
	}
	return nil
}

// Parse decodes and validates kb/entities.json. Empty input and "{}" are an
// empty version-1 map.
func Parse(data []byte) (Map, error) {
	empty := Map{Version: Version, Entities: []Entity{}}
	if len(bytes.TrimSpace(data)) == 0 {
		return empty, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Map{}, fmt.Errorf("entity map: %w", err)
	}
	if len(raw) == 0 {
		return empty, nil
	}
	var m Map
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return Map{}, fmt.Errorf("entity map: %w", err)
	}
	if m.Entities == nil {
		m.Entities = []Entity{}
	}
	return m, Validate(m)
}

// LoadFile reads an entity map file. A missing file is an empty map.
func LoadFile(name string) (Map, error) {
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return Parse(nil)
	}
	if err != nil {
		return Map{}, err
	}
	return Parse(data)
}

// Encode validates m and returns its canonical form: entities sorted by ID,
// aliases, owners and part_of sorted, absent lists written as [], two-space
// indentation and a trailing newline. Path order is kept because the first
// path is the primary one.
func Encode(m Map) ([]byte, error) {
	if err := Validate(m); err != nil {
		return nil, err
	}
	out := Map{Version: m.Version, Entities: make([]Entity, 0, len(m.Entities))}
	for _, e := range m.Entities {
		out.Entities = append(out.Entities, Entity{ID: e.ID, Name: e.Name, Aliases: sorted(e.Aliases), Paths: append([]string{}, e.Paths...), Owners: sorted(e.Owners), PartOf: sorted(e.PartOf)})
	}
	sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].ID < out.Entities[j].ID })
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sorted(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}
