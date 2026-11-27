package verification

import (
	"errors"
	"path"
	"strings"
)

// CheckGlob accepts a repository-relative glob: slash-separated segments, each
// a path.Match pattern or "**" for any number of segments.
func CheckGlob(pattern string) error {
	if pattern == "" || strings.ContainsAny(pattern, "\x00\r\n") {
		return errors.New("glob must be non-empty and one line")
	}
	if path.IsAbs(pattern) {
		return errors.New("glob must be relative")
	}
	for _, segment := range strings.Split(pattern, "/") {
		switch {
		case segment == "" || segment == "." || segment == "..":
			return errors.New("glob has an empty, . or .. segment")
		case segment != "**" && strings.Contains(segment, "**"):
			return errors.New("glob uses ** inside a segment")
		}
		if _, err := path.Match(segment, ""); err != nil {
			return errors.New("glob is not valid")
		}
	}
	return nil
}

// MatchGlob reports whether a repository path lies under a glob: the segments
// of the glob match the leading segments of the path with path.Match, "**"
// matches any number of segments, and everything beneath a matched directory
// matches too.
func MatchGlob(pattern, name string) bool {
	if CheckGlob(pattern) != nil || name == "" || path.IsAbs(name) || path.Clean(name) != name {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return true
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegments(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], name[0])
	return ok && err == nil && matchSegments(pattern[1:], name[1:])
}

// LiteralPrefix is the part of a glob before its first glob character.
func LiteralPrefix(pattern string) string {
	if i := strings.IndexAny(pattern, "*?[\\"); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// PatternsMayOverlap is the conservative overlap rule for path patterns that
// may name files that do not exist yet: two patterns overlap when the literal
// prefix of one, before its first glob character, is a prefix of the other's.
// It names too many overlaps rather than too few. Invalid globs never overlap.
func PatternsMayOverlap(a, b string) bool {
	if CheckGlob(a) != nil || CheckGlob(b) != nil {
		return false
	}
	pa, pb := LiteralPrefix(a), LiteralPrefix(b)
	return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
}
