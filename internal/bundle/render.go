package bundle

import (
	"fmt"
	"strings"
)

// Render returns the bundle as the text a turn request carries. The output
// depends only on the bundle, and each section header names the path and
// record it was read from.
func (b Bundle) Render() string {
	var w strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&w, format+"\n", args...) }
	line("# Project context")
	line("")
	line("project: %s", b.Project)
	line("context mode: %s", b.Mode)
	if len(b.Scope.Entities) == 0 {
		line("scope: whole project")
	} else {
		line("scope: entities %s", strings.Join(b.Scope.Entities, ", "))
	}
	if b.Scope.Workstream != "" {
		line("workstream: %s", b.Scope.Workstream)
	}

	line("")
	line("## Charter")
	line("source: %s (record %s revision %d)", b.Charter.Source, b.Charter.Record, b.Charter.Revision)
	if len(b.Charter.Rules) == 0 {
		line("The charter has no rules.")
	}
	for _, r := range b.Charter.Rules {
		if r.Heading != "" {
			line("- charter#%d [%s]: %s", r.Number, r.Heading, r.Text)
		} else {
			line("- charter#%d: %s", r.Number, r.Text)
		}
	}
	for _, d := range b.Charter.Diagnostics {
		line("- note: %s line %d: %s", b.Charter.Source, d.Line, d.Message)
	}

	line("")
	line("## Knowledge base")
	if len(b.Knowledge) == 0 && len(b.Missing) == 0 {
		line("No knowledge-base prose exists.")
	}
	for _, m := range b.Missing {
		line("- missing: no prose for entity %s; looked for %s", m.Entity, strings.Join(m.Looked, ", "))
	}
	for _, p := range b.Knowledge {
		line("")
		line("### %s (subsystem %s)", p.Source, p.Subsystem)
		w.WriteString(p.Content)
		if !strings.HasSuffix(p.Content, "\n") {
			w.WriteString("\n")
		}
		line("### end of %s", p.Source)
	}

	line("")
	line("## Entities")
	if b.Entities.Revision == 0 {
		line("source: %s (no recorded revision)", b.Entities.Source)
	} else {
		line("source: %s (record %s revision %d)", b.Entities.Source, b.Entities.Record, b.Entities.Revision)
	}
	if len(b.Entities.Entities) == 0 {
		line("No entities.")
	}
	for _, e := range b.Entities.Entities {
		paths := "no paths"
		if len(e.Paths) > 0 {
			paths = strings.Join(e.Paths, ", ")
		}
		line("- %s (%s): %s", e.ID, e.Name, paths)
	}
	if len(b.Entities.Unresolved) > 0 {
		line("- unresolved: %s", strings.Join(b.Entities.Unresolved, ", "))
	}

	line("")
	line("## Decisions")
	if len(b.Decisions) == 0 {
		line("No rulings are recorded.")
	}
	for _, d := range b.Decisions {
		line("- %s (record %s revision %d, question %s revision %d)", d.Source, d.Record, d.Revision, d.QuestionID, d.QuestionRevision)
		line("  decision: %s", indent(d.Decision))
		if d.OwnerResponse != "" {
			line("  owner: %s", indent(d.OwnerResponse))
		}
		if d.ReturnedAnswer == "" {
			line("  answer: waiting for the chief of staff to relay the ruling")
		} else {
			line("  answer: %s", indent(d.ReturnedAnswer))
		}
	}

	line("")
	line("## Notices")
	if len(b.Notices) == 0 {
		line("No project-wide notices.")
	}
	for _, n := range b.Notices {
		line("- %s (record %s revision %d, workstream %s)", n.Source, n.Record, n.Revision, n.Workstream)
		line("  notice: %s", indent(n.Text))
	}
	return w.String()
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
