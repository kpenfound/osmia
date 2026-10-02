// Package gitdiff reads the unified diffs git diff --no-renames writes.
package gitdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// File is one file's section of a unified git diff.
type File struct {
	Path    string
	Header  []string
	Hunks   []Hunk
	Binary  bool
	Added   int
	Removed int
}

// Hunk is one hunk and the changed-side lines it covers.
type Hunk struct {
	Lines      []string
	Start, End int
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// Parse splits the output of git diff --no-renames into files. Lines keep
// their endings; a binary patch body is dropped.
func Parse(diff string) []File {
	var files []File
	binaryBody := false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			files = append(files, File{Path: path(strings.TrimSuffix(line, "\n")), Header: []string{line}})
			binaryBody = false
			continue
		}
		if len(files) == 0 || binaryBody {
			continue
		}
		f := &files[len(files)-1]
		switch {
		case strings.HasPrefix(line, "@@ "):
			h := Hunk{Lines: []string{line}}
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				start, _ := strconv.Atoi(m[1])
				count := 1
				if m[2] != "" {
					count, _ = strconv.Atoi(m[2])
				}
				h.Start, h.End = max(start, 1), max(start+count-1, start, 1)
			}
			f.Hunks = append(f.Hunks, h)
		case len(f.Hunks) != 0:
			h := &f.Hunks[len(f.Hunks)-1]
			h.Lines = append(h.Lines, line)
			switch line[0] {
			case '+':
				f.Added++
			case '-':
				f.Removed++
			}
		case strings.TrimSuffix(line, "\n") == "GIT binary patch":
			f.Binary, binaryBody = true, true
		default:
			if strings.HasPrefix(line, "Binary files ") {
				f.Binary = true
			}
			f.Header = append(f.Header, line)
		}
	}
	return files
}

// path reads the path of a "diff --git a/<path> b/<path>" line, whose two
// paths are equal without rename detection.
func path(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if strings.HasPrefix(rest, `"`) {
		if quoted, err := strconv.QuotedPrefix(rest); err == nil {
			if path, err := strconv.Unquote(quoted); err == nil {
				return strings.TrimPrefix(path, "a/")
			}
		}
	}
	n := (len(rest) - len("a/ b/")) / 2
	if n <= 0 || !strings.HasPrefix(rest, "a/") {
		return rest
	}
	return rest[len("a/") : len("a/")+n]
}

// ChangedFiles lists a diff's files with their added and removed lines, one
// per line, for a prompt or a judgment's state.
func ChangedFiles(diff string) string {
	var out strings.Builder
	for _, f := range Parse(diff) {
		fmt.Fprintf(&out, "- %s (+%d -%d)\n", f.Path, f.Added, f.Removed)
	}
	if out.Len() == 0 {
		return "(no changed files)\n"
	}
	return out.String()
}
