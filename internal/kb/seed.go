package kb

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// CodeownersFiles are the locations GitHub reads, in its order of precedence.
var CodeownersFiles = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// Containers are the top-level directories whose subdirectories are seeded as
// entities of their own.
var Containers = []string{"apps", "cmd", "crates", "internal", "libs", "packages", "pkg", "services"}

// rule is one CODEOWNERS line translated to an entity path pattern.
type rule struct {
	pattern string
	literal bool
	owners  []string
}

// Seed builds the entity map of a clone from its CODEOWNERS file and its
// directory structure. It only reads the clone, never follows a symlink out
// of it, and returns the same map for the same files.
func Seed(clone string) (Map, error) {
	root, err := os.OpenRoot(clone)
	if err != nil {
		return Map{}, err
	}
	defer root.Close()
	fsys := root.FS()
	rules, err := codeowners(fsys)
	if err != nil {
		return Map{}, err
	}
	paths := map[string]bool{}
	top, err := directories(fsys, ".")
	if err != nil {
		return Map{}, err
	}
	for _, dir := range top {
		paths[dir] = true
		if !contains(Containers, dir) {
			continue
		}
		children, err := directories(fsys, dir)
		if err != nil {
			return Map{}, err
		}
		for _, child := range children {
			paths[dir+"/"+child] = true
		}
	}
	for _, r := range rules {
		if !r.literal || hidden(r.pattern) {
			continue
		}
		if _, err := fs.Stat(fsys, r.pattern); err == nil {
			paths[r.pattern] = true
		}
	}
	return build(paths, rules), nil
}

func build(set map[string]bool, rules []rule) Map {
	var paths []string
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	ids := map[string]string{}
	taken := map[string]bool{}
	for _, p := range paths {
		id := ID(p)
		for n := 2; taken[id]; n++ {
			id = fmt.Sprintf("%s-%d", ID(p), n)
		}
		taken[id] = true
		ids[p] = id
	}
	bases := map[string]int{}
	for _, p := range paths {
		if strings.Contains(p, "/") {
			bases[strings.ToLower(path.Base(p))]++
		}
	}
	m := Map{Version: Version, Entities: []Entity{}}
	for _, p := range paths {
		e := Entity{ID: ids[p], Name: p, Aliases: []string{}, Paths: []string{p}, Owners: []string{}, PartOf: []string{}}
		if base := path.Base(p); strings.Contains(p, "/") && bases[strings.ToLower(base)] == 1 && !taken[strings.ToLower(base)] {
			e.Aliases = append(e.Aliases, base)
		}
		segments := strings.Split(p, "/")
		for i := len(rules) - 1; i >= 0; i-- {
			if match(strings.Split(rules[i].pattern, "/"), segments) {
				e.Owners = append(e.Owners, rules[i].owners...)
				break
			}
		}
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if id, ok := ids[parent]; ok {
				e.PartOf = append(e.PartOf, id)
				break
			}
		}
		m.Entities = append(m.Entities, e)
	}
	return m
}

// directories lists the visible subdirectories of dir, excluding symlinks and
// names that would read as glob patterns.
func directories(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, "*?[]\\\x00\r\n") {
			names = append(names, name)
		}
	}
	return names, nil
}

func hidden(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if strings.HasPrefix(s, ".") {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// codeowners reads the first CODEOWNERS location that holds a regular file
// inside the clone. Lines whose pattern cannot be expressed as an entity path
// pattern are skipped.
func codeowners(fsys fs.FS) ([]rule, error) {
	for _, name := range CodeownersFiles {
		info, err := fs.Stat(fsys, name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		return parseCodeowners(data), nil
	}
	return nil, nil
}

func parseCodeowners(data []byte) []rule {
	var rules []rule
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pattern, literal, ok := translate(fields[0])
		if !ok {
			continue
		}
		rules = append(rules, rule{pattern: pattern, literal: literal, owners: fields[1:]})
	}
	return rules
}

// translate turns a CODEOWNERS pattern into an entity path pattern. A pattern
// with a leading or inner slash is anchored at the repository root; any other
// pattern matches at any depth. A trailing slash or "/**" is dropped because an
// entity pattern already covers everything below the paths it matches.
func translate(p string) (pattern string, literal, ok bool) {
	anchored := strings.Contains(strings.TrimSuffix(p, "/"), "/")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, "/**")
	if p == "" || p == "**" {
		return "**", false, true
	}
	if !anchored && !strings.HasPrefix(p, "**/") {
		p = "**/" + p
	}
	if CheckPattern(p) != nil {
		return "", false, false
	}
	return p, !strings.ContainsAny(p, "*?[\\"), true
}
