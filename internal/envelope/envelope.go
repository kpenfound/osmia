// Package envelope renders attributed service sections for answers and notices.
package envelope

import (
	"fmt"
	"strings"
)

// Section is one named part of an answer or notice. Text is kept byte for byte;
// the renderer marks every content line as data.
type Section struct {
	Name string
	Text string
}

var attribution = map[string]string{
	"question":        "asker (copied by Osmia)",
	"answer":          "chief of staff",
	"owner_response":  "owner (copied by Osmia)",
	"returned_answer": "chief of staff relay",
	"citations":       "chief of staff",
	"charter_rule":    "chief of staff proposal, ratified by the owner",
}

// Render validates structural names and renders content with a data prefix on
// every line. A byte count distinguishes a trailing newline from an empty line.
func Render(sections ...Section) (string, error) {
	seen := map[string]bool{}
	var b strings.Builder
	for _, s := range sections {
		who, ok := attribution[s.Name]
		if !ok {
			return "", fmt.Errorf("unknown envelope section %q", s.Name)
		}
		if seen[s.Name] {
			return "", fmt.Errorf("duplicate envelope section %q", s.Name)
		}
		seen[s.Name] = true
		fmt.Fprintf(&b, "<<< osmia:%s | %s | bytes=%d >>>\n", s.Name, who, len(s.Text))
		for _, line := range strings.Split(s.Text, "\n") {
			fmt.Fprintf(&b, "| %s\n", strings.ReplaceAll(strings.ReplaceAll(line, "\\", "\\\\"), "\r", "\\r"))
		}
		fmt.Fprintf(&b, "<<< /osmia:%s >>>\n", s.Name)
	}
	return b.String(), nil
}
