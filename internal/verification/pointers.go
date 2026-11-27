package verification

import (
	"io/fs"
	"path"
	"strings"
)

// MaxWalkEntries bounds how many tree entries Applicable reads for one path
// pattern. A pattern whose subtree is larger is treated as naming files that
// may not exist, which names more features rather than fewer.
const MaxWalkEntries = 20000

// Applicable returns the features a unit may touch, in the order of the map,
// by plain glob matching with no model call. A feature applies when:
//   - a file of tree matches both one of the path patterns and one of the
//     feature's paths globs;
//   - a path pattern matches no file of tree, so may name files that do not
//     exist yet, and may overlap a feature glob by PatternsMayOverlap;
//   - one of the changed files matches a feature's paths globs.
//
// A pattern that covers the whole repository applies every feature. tree is the
// tree the patterns are expanded against and may be nil.
func (m FeatureMap) Applicable(tree fs.FS, patterns, changed []string) []Feature {
	var matched, unmatched []string
	for _, pattern := range patterns {
		if wholeRepository(pattern) {
			return append([]Feature(nil), m.Features...)
		}
		files, ok := expand(tree, pattern)
		if !ok || len(files) == 0 {
			unmatched = append(unmatched, pattern)
			continue
		}
		matched = append(matched, files...)
	}
	matched = append(matched, changed...)
	var out []Feature
	for _, f := range m.Features {
		if matchesAny(f.Paths, matched, MatchGlob) || matchesAny(f.Paths, unmatched, PatternsMayOverlap) {
			out = append(out, f)
		}
	}
	return out
}

func wholeRepository(pattern string) bool {
	return strings.Trim(pattern, "*/") == "" && strings.Contains(pattern, "*")
}

func expand(tree fs.FS, pattern string) ([]string, bool) {
	if tree == nil || CheckGlob(pattern) != nil {
		return nil, false
	}
	var literal []string
	for _, segment := range strings.Split(pattern, "/") {
		if strings.ContainsAny(segment, "*?[\\") {
			break
		}
		literal = append(literal, segment)
	}
	start := "."
	if len(literal) > 0 {
		start = path.Join(literal...)
	}
	info, err := fs.Stat(tree, start)
	if err != nil {
		return nil, true
	}
	if !info.IsDir() {
		if MatchGlob(pattern, start) {
			return []string{start}, true
		}
		return nil, true
	}
	var files []string
	entries := 0
	exceeded := false
	_ = fs.WalkDir(tree, start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fs.SkipDir
		}
		if entries++; entries > MaxWalkEntries {
			exceeded = true
			return fs.SkipAll
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if MatchGlob(pattern, p) {
			files = append(files, p)
		}
		return nil
	})
	if exceeded {
		return nil, false
	}
	return files, true
}
