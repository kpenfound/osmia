// Package charter parses a project charter into citable rules.
package charter

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Rule is one numbered rule, cited as charter#<Number>. Heading is the text of
// the nearest heading above it, empty before the first heading.
type Rule struct {
	Number  int    `json:"number"`
	Text    string `json:"text"`
	Heading string `json:"heading"`
}

// Diagnostic describes a numbering problem. It never prevents parsing.
type Diagnostic struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type Charter struct {
	Rules       []Rule       `json:"rules"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Empty reports whether the charter has no rules. Guidance text alone is empty.
func (c Charter) Empty() bool { return len(c.Rules) == 0 }

// Rule returns the rule cited as charter#n. A number held by more than one
// rule is not citable.
func (c Charter) Rule(n int) (Rule, bool) {
	var found []Rule
	for _, r := range c.Rules {
		if r.Number == n {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		return Rule{}, false
	}
	return found[0], true
}

var (
	item    = regexp.MustCompile(`^ {0,3}([0-9]{1,9})\.(?:[ \t]+(.*))?$`)
	heading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	fence   = regexp.MustCompile("^ {0,3}(```|~~~)")
	bullet  = regexp.MustCompile(`^ {0,3}(?:[-*+]|[0-9]{1,9}\))(?:[ \t]|$)`)
)

// Parse returns the rules of a charter and its numbering diagnostics. A rule
// is a Markdown ordered-list item written as "N. text" at any heading level,
// numbered by its own list number. Lines that follow an item continue its
// text until a line that is blank once comments are removed, a heading, a
// bullet or a code fence. Headings, other text, HTML comments and fenced code
// are not rules. Duplicate numbers, gaps and items without text are reported.
func Parse(content string) Charter {
	c := Charter{Rules: []Rule{}, Diagnostics: []Diagnostic{}}
	lines := make(map[int][]int) // rule number -> source lines
	var (
		section string
		open    = -1 // index of the rule still accepting continuation lines
		comment bool
		code    string
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
		switch m := item.FindStringSubmatch(line); {
		case text == "":
			open = -1
		case heading.MatchString(line):
			section, open = strings.TrimSpace(strings.TrimRight(heading.FindStringSubmatch(line)[2], "#")), -1
		case m != nil:
			number, _ := strconv.Atoi(m[1])
			body := strings.TrimSpace(m[2])
			if body == "" {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{n, fmt.Sprintf("item %d has no text and is not a rule", number)})
				open = -1
				continue
			}
			c.Rules = append(c.Rules, Rule{number, body, section})
			lines[number] = append(lines[number], n)
			open = len(c.Rules) - 1
		case open >= 0 && !bullet.MatchString(line):
			c.Rules[open].Text += " " + text
		default:
			open = -1
		}
	}
	numbers := slices.Sorted(maps.Keys(lines))
	previous := 0
	for _, number := range numbers {
		at := lines[number]
		if number > previous+1 {
			missing := fmt.Sprintf("rule %d is", previous+1)
			if number > previous+2 {
				missing = fmt.Sprintf("rules %d-%d are", previous+1, number-1)
			}
			c.Diagnostics = append(c.Diagnostics, Diagnostic{at[0], fmt.Sprintf("numbering gap: %s missing before rule %d", missing, number)})
		}
		if len(at) > 1 {
			c.Diagnostics = append(c.Diagnostics, Diagnostic{at[1], fmt.Sprintf("rule %d is numbered %d times (lines %s); it cannot be cited until renumbered", number, len(at), joinInts(at))})
		}
		previous = number
	}
	slices.SortStableFunc(c.Diagnostics, func(a, b Diagnostic) int { return a.Line - b.Line })
	return c
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
