package charter

import (
	"regexp"
	"strconv"
)

var citation = regexp.MustCompile(`^(?:([A-Za-z0-9][A-Za-z0-9_-]*)/)?charter#([1-9][0-9]{0,8})$`)
var references = regexp.MustCompile(`(?:[A-Za-z0-9][A-Za-z0-9_-]*/)?charter#[0-9]+`)

// Citation parses a charter citation. Project is empty for the citing member's charter.
// Qualified citations use the project's p_ ID, as displayed in turn context.
func Citation(text string) (project string, number int, ok bool) {
	m := citation.FindStringSubmatch(text)
	if m == nil {
		return "", 0, false
	}
	n, _ := strconv.Atoi(m[2])
	return m[1], n, true
}

// References returns the charter citations embedded in document text.
func References(text string) []string { return references.FindAllString(text, -1) }
