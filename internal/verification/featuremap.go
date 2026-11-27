package verification

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// MaxHeaderBytes caps how much of a feature file the reader looks at; a header
// longer than this is unparseable.
const MaxHeaderBytes = 32 << 10

// Feature is one feature-map file's header.
type Feature struct {
	// File is the repository-relative path of the feature file.
	File string `json:"file"`
	// Name is the header's feature name.
	Name string `json:"feature"`
	// Paths are the repository globs the feature covers.
	Paths []string `json:"paths"`
	// Flows are the lever flow names the feature exercises.
	Flows []string `json:"flows"`
}

// Note is a structure-check finding about the map: it informs a reviewer and
// blocks nothing.
type Note struct {
	File    string `json:"file,omitempty"`
	Message string `json:"message"`
}

// String is the note as one line.
func (n Note) String() string {
	if n.File == "" {
		return n.Message
	}
	return n.File + ": " + n.Message
}

// FeatureMap is the parsed feature-map directory. Features holds every file
// whose header parsed, in file-name order. Notes holds the files and problems
// found while reading.
type FeatureMap struct {
	Dir      string    `json:"dir"`
	Features []Feature `json:"features,omitempty"`
	Notes    []Note    `json:"notes,omitempty"`
}

// ReadMap reads dir/*.md from fsys, whose root is the repository. README.md is
// the map's index and is not a feature. A directory that cannot be read or a
// file whose header does not parse is a note, never an error, and the other
// features are still returned.
func ReadMap(fsys fs.FS, dir string) FeatureMap {
	m := FeatureMap{Dir: dir}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		m.Notes = append(m.Notes, Note{File: dir, Message: "the feature map directory cannot be read"})
		return m
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.EqualFold(name, "README.md") {
			continue
		}
		file := path.Join(dir, name)
		data, err := readHead(fsys, file)
		if err != nil {
			m.Notes = append(m.Notes, Note{File: file, Message: "the feature file cannot be read"})
			continue
		}
		feature, err := ParseHeader(data)
		if err != nil {
			m.Notes = append(m.Notes, Note{File: file, Message: "the header does not parse: " + err.Error()})
			continue
		}
		feature.File = file
		m.Features = append(m.Features, feature)
	}
	return m
}

// ReadMapDir reads the feature map of the repository checked out at repo. No
// path in the map directory can lead outside repo.
func ReadMapDir(repo, dir string) FeatureMap {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return FeatureMap{Dir: dir, Notes: []Note{{File: dir, Message: "the feature map directory cannot be read"}}}
	}
	defer root.Close()
	return ReadMap(root.FS(), dir)
}

func readHead(fsys fs.FS, file string) ([]byte, error) {
	f, err := fsys.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxHeaderBytes+1))
}

// ParseHeader parses the frontmatter at the start of a feature file: a block
// between two --- lines of `feature`, `paths` and `flows`, where the lists are
// written inline as [a, b] or as - items on following lines. Other keys and
// the body are ignored. The feature name and at least one valid path glob are
// required.
func ParseHeader(data []byte) (Feature, error) {
	if len(data) > MaxHeaderBytes {
		return Feature{}, errors.New("the file is too large to read a header from")
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return Feature{}, errors.New("the file is not UTF-8 text")
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return Feature{}, errors.New("the file does not start with a --- header")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Feature{}, errors.New("the header has no closing ---")
	}
	values := map[string][]string{}
	scalars := map[string]bool{}
	var key string
	for _, line := range lines[1:end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' && !strings.HasPrefix(trimmed, "- ") && trimmed != "-" {
			name, value, ok := strings.Cut(line, ":")
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return Feature{}, errors.New("a header line is not key: value")
			}
			if _, dup := values[name]; dup {
				return Feature{}, fmt.Errorf("the header sets %s twice", name)
			}
			key = name
			value = strings.TrimSpace(value)
			values[key] = []string{}
			switch {
			case value == "":
			case strings.HasPrefix(value, "["):
				items, err := inlineList(value)
				if err != nil {
					return Feature{}, fmt.Errorf("%s: %w", name, err)
				}
				values[key] = items
			default:
				scalars[key] = true
				values[key] = []string{unquote(value)}
			}
			continue
		}
		item, ok := strings.CutPrefix(trimmed, "-")
		if key == "" || !ok || scalars[key] {
			return Feature{}, errors.New("a header line is not part of a list")
		}
		values[key] = append(values[key], unquote(strings.TrimSpace(item)))
	}
	names := values["feature"]
	if len(names) != 1 || strings.TrimSpace(names[0]) == "" || !scalars["feature"] {
		return Feature{}, errors.New("the header has no feature name")
	}
	f := Feature{Name: names[0]}
	f.Paths = values["paths"]
	if len(f.Paths) == 0 {
		return Feature{}, errors.New("the header has no paths")
	}
	for _, p := range f.Paths {
		if err := CheckGlob(p); err != nil {
			return Feature{}, fmt.Errorf("paths entry %q is not a repository glob", boundedText(p))
		}
	}
	f.Flows = []string{}
	seen := map[string]bool{}
	for _, flow := range values["flows"] {
		if strings.TrimSpace(flow) == "" {
			return Feature{}, errors.New("the header has an empty flow name")
		}
		if !seen[flow] {
			seen[flow] = true
			f.Flows = append(f.Flows, flow)
		}
	}
	return f, nil
}

func inlineList(value string) ([]string, error) {
	inner, ok := strings.CutSuffix(strings.TrimPrefix(value, "["), "]")
	if !ok {
		return nil, errors.New("an inline list is not closed with ]")
	}
	var items []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if item := strings.TrimSpace(current.String()); item != "" {
			items = append(items, unquote(item))
		}
		current.Reset()
	}
	for _, r := range inner {
		switch {
		case quote != 0:
			current.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			current.WriteRune(r)
		case r == ',':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, errors.New("an inline list has an unclosed quote")
	}
	flush()
	return items, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// MatchFiles returns the features whose paths globs match at least one of the
// concrete repository files, by plain glob matching.
func (m FeatureMap) MatchFiles(files []string) []Feature {
	var out []Feature
	for _, f := range m.Features {
		if matchesAny(f.Paths, files, MatchGlob) {
			out = append(out, f)
		}
	}
	return out
}

// MatchPatterns returns the features with a paths glob that may overlap one of
// the path patterns, by the conservative literal-prefix rule of
// PatternsMayOverlap. It serves patterns that may name files that do not exist
// yet, and names too many features rather than too few.
func (m FeatureMap) MatchPatterns(patterns []string) []Feature {
	var out []Feature
	for _, f := range m.Features {
		if matchesAny(f.Paths, patterns, PatternsMayOverlap) {
			out = append(out, f)
		}
	}
	return out
}

func matchesAny(globs, subjects []string, match func(glob, subject string) bool) bool {
	for _, glob := range globs {
		for _, subject := range subjects {
			if match(glob, subject) {
				return true
			}
		}
	}
	return false
}

// UnionFlows returns every flow the features name, once each, in the order of
// the features and then of their headers.
func UnionFlows(features []Feature) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range features {
		for _, flow := range f.Flows {
			if !seen[flow] {
				seen[flow] = true
				out = append(out, flow)
			}
		}
	}
	return out
}

// Flows is the union of flows across the whole map.
func (m FeatureMap) Flows() []string { return UnionFlows(m.Features) }

// Check is the structure check: the notes found while reading the map plus a
// note for each flow a header names that the lever's list does not. A nil
// listed means the list could not be obtained, so no flow is compared. Check
// never fails: its notes inform a reviewer and block nothing.
func (m FeatureMap) Check(listed []string) []Note {
	notes := append([]Note(nil), m.Notes...)
	if listed != nil {
		known := map[string]bool{}
		for _, name := range listed {
			known[name] = true
		}
		for _, f := range m.Features {
			for _, flow := range f.Flows {
				if !known[flow] {
					notes = append(notes, Note{File: f.File, Message: fmt.Sprintf("flow %q is not in the lever's list", boundedText(flow))})
				}
			}
		}
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].File < notes[j].File })
	return notes
}

// ReadIndex returns the text of the map directory's README.md, the index that
// lists the features in the order of a full regression sweep. It returns false
// when the file cannot be read.
func ReadIndex(fsys fs.FS, dir string) (string, bool) {
	data, err := readHead(fsys, path.Join(dir, "README.md"))
	if err != nil || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// Ordered returns the features in the order the index lists them: each list
// item of the index names the feature whose file name or feature name appears
// first in it, and a feature the index does not name follows, in file-name
// order. An index that cannot be read leaves file-name order.
func (m FeatureMap) Ordered(index string) []Feature {
	remaining := append([]Feature(nil), m.Features...)
	var out []Feature
	for _, line := range strings.Split(index, "\n") {
		if !isListItem(line) {
			continue
		}
		lower := strings.ToLower(line)
		best, at := -1, len(lower)+1
		for i, f := range remaining {
			if pos := firstMention(lower, f); pos >= 0 && pos < at {
				best, at = i, pos
			}
		}
		if best >= 0 {
			out = append(out, remaining[best])
			remaining = append(remaining[:best], remaining[best+1:]...)
		}
	}
	return append(out, remaining...)
}

func isListItem(line string) bool {
	line = strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(line, "-"); ok {
		return strings.HasPrefix(rest, " ")
	}
	for _, bullet := range []string{"*", "+"} {
		if rest, ok := strings.CutPrefix(line, bullet); ok {
			return strings.HasPrefix(rest, " ")
		}
	}
	digits := strings.TrimLeft(line, "0123456789")
	if digits == line {
		return false
	}
	return strings.HasPrefix(digits, ". ") || strings.HasPrefix(digits, ") ")
}

func firstMention(line string, f Feature) int {
	base := strings.ToLower(strings.TrimSuffix(path.Base(f.File), ".md"))
	best := -1
	for _, key := range []string{base, strings.ToLower(f.Name)} {
		if key == "" {
			continue
		}
		if pos := indexWord(line, key); pos >= 0 && (best < 0 || pos < best) {
			best = pos
		}
	}
	return best
}

func indexWord(line, key string) int {
	for from := 0; from < len(line); {
		i := strings.Index(line[from:], key)
		if i < 0 {
			return -1
		}
		i += from
		end := i + len(key)
		if (i == 0 || !wordByte(line[i-1])) && (end == len(line) || !wordByte(line[end])) {
			return i
		}
		from = i + 1
	}
	return -1
}

func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}
