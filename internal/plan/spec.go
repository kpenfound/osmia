// Package plan holds a workstream's feature spec and plan: the acceptance
// criteria parsed from spec.md, the typed plan.json, and the validator that
// decides whether a plan may be presented or ratified.
package plan

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Trace document identities of the two workstream documents.
const (
	SpecDocument = "spec"
	SpecPath     = "spec.md"
	PlanDocument = "plan"
	PlanPath     = "plan.json"
)

// CriteriaHeading is the heading whose section holds the acceptance criteria.
const CriteriaHeading = "Acceptance criteria"

// Criterion is one acceptance criterion, cited as spec#<Number>.
type Criterion struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

// Diagnostic describes a problem with the criteria list. It never prevents
// parsing. Line is zero for a problem with the whole document, and Criterion
// is the number concerned, or zero; a gap names its first missing number.
type Diagnostic struct {
	Line      int    `json:"line"`
	Criterion int    `json:"criterion,omitempty"`
	Message   string `json:"message"`
}

// Spec is what Osmia reads from spec.md.
type Spec struct {
	Criteria    []Criterion  `json:"criteria"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Criterion returns the criterion cited as spec#n. A number held by more than
// one criterion is not citable.
func (s Spec) Criterion(n int) (Criterion, bool) {
	found := s.numbered(n)
	if len(found) != 1 {
		return Criterion{}, false
	}
	return found[0], true
}

// Cite returns the citation of criterion n: spec#n.
func Cite(n int) string { return "spec#" + strconv.Itoa(n) }

var citation = regexp.MustCompile(`^spec#([1-9][0-9]{0,8})$`)

// ParseCitation returns the criterion number of a spec#<n> citation.
func ParseCitation(s string) (int, bool) {
	m := citation.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

var (
	item    = regexp.MustCompile(`^ {0,3}([0-9]{1,9})\.(?:[ \t]+(.*))?$`)
	heading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	fence   = regexp.MustCompile("^ {0,3}(```|~~~)")
	bullet  = regexp.MustCompile(`^ {0,3}(?:[-*+]|[0-9]{1,9}\))(?:[ \t]|$)`)
)

// ParseSpec returns the acceptance criteria of a spec and their diagnostics.
// Criteria are the Markdown ordered-list items written as "N. text" in the
// section headed "Acceptance criteria" (any level, ignoring case), which ends at
// the next heading of the same or a higher level. Each criterion keeps its own
// list number. Lines that follow an item without a blank line continue its
// text. Fenced code and HTML comments are ignored. A missing or repeated
// section, an empty criteria list, duplicate numbers, missing numbers and items
// without text are reported.
func ParseSpec(content string) Spec {
	s := Spec{Criteria: []Criterion{}, Diagnostics: []Diagnostic{}}
	lines := map[int][]int{} // criterion number -> source lines
	var (
		level    int // heading level of the criteria section, 0 outside it
		sections []int
		open     = -1 // index of the criterion still accepting continuation lines
		comment  bool
		code     string
	)
	for i, raw := range strings.Split(content, "\n") {
		n := i + 1
		line := strings.TrimRight(raw, "\r")
		if code != "" {
			if strings.HasPrefix(strings.TrimSpace(line), code) {
				code = ""
			}
			continue
		}
		line, comment = stripComments(line, comment)
		if m := fence.FindStringSubmatch(line); m != nil {
			code, open = m[1], -1
			continue
		}
		text := strings.TrimSpace(line)
		if h := heading.FindStringSubmatch(line); h != nil {
			open = -1
			depth := len(h[1])
			if level > 0 && depth <= level {
				level = 0
			}
			title := strings.TrimSpace(strings.TrimRight(h[2], "#"))
			if strings.EqualFold(title, CriteriaHeading) {
				level = depth
				sections = append(sections, n)
			}
			continue
		}
		if level == 0 {
			continue
		}
		switch m := item.FindStringSubmatch(line); {
		case text == "":
			open = -1
		case m != nil:
			number, _ := strconv.Atoi(m[1])
			body := strings.TrimSpace(m[2])
			if body == "" {
				s.Diagnostics = append(s.Diagnostics, Diagnostic{n, number, fmt.Sprintf("item %d has no text and is not a criterion", number)})
				open = -1
				continue
			}
			s.Criteria = append(s.Criteria, Criterion{number, body})
			lines[number] = append(lines[number], n)
			open = len(s.Criteria) - 1
		case open >= 0 && !bullet.MatchString(line):
			s.Criteria[open].Text += " " + text
		default:
			open = -1
		}
	}
	switch {
	case len(sections) == 0:
		s.Diagnostics = append(s.Diagnostics, Diagnostic{0, 0, fmt.Sprintf("no %q section", CriteriaHeading)})
	case len(s.Criteria) == 0:
		s.Diagnostics = append(s.Diagnostics, Diagnostic{sections[0], 0, fmt.Sprintf("%q section has no numbered criteria", CriteriaHeading)})
	}
	if len(sections) > 1 {
		s.Diagnostics = append(s.Diagnostics, Diagnostic{sections[1], 0, fmt.Sprintf("%q section appears %d times (lines %s)", CriteriaHeading, len(sections), joinInts(sections))})
	}
	previous := 0
	for _, number := range slices.Sorted(maps.Keys(lines)) {
		at := lines[number]
		if number > previous+1 {
			missing := fmt.Sprintf("criterion %d is", previous+1)
			if number > previous+2 {
				missing = fmt.Sprintf("criteria %d-%d are", previous+1, number-1)
			}
			s.Diagnostics = append(s.Diagnostics, Diagnostic{at[0], previous + 1, fmt.Sprintf("numbering gap: %s missing before criterion %d", missing, number)})
		}
		if len(at) > 1 {
			s.Diagnostics = append(s.Diagnostics, Diagnostic{at[1], number, fmt.Sprintf("criterion %d is numbered %d times (lines %s); it cannot be cited until renumbered", number, len(at), joinInts(at))})
		}
		previous = number
	}
	slices.SortStableFunc(s.Diagnostics, func(a, b Diagnostic) int { return a.Line - b.Line })
	return s
}

// stripComments removes HTML comment text from a line, given whether a
// comment is already open, and reports whether one is still open at its end.
func stripComments(line string, open bool) (string, bool) {
	var out strings.Builder
	for {
		if open {
			end := strings.Index(line, "-->")
			if end < 0 {
				return out.String(), true
			}
			line, open = line[end+3:], false
			continue
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			out.WriteString(line)
			return out.String(), false
		}
		out.WriteString(line[:start])
		line, open = line[start+4:], true
	}
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}
