package bundle

import (
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/envelope"
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
	if spec := b.ProjectSpec; spec != nil {
		if spec.Draft {
			line("\n## Draft project spec")
		} else {
			line("\n## Ratified project spec")
		}
		line("source: %s (record %s, workgroup %s)", spec.Source, spec.Record, spec.Workgroup)
		line("%s", spec.Behavior)
		for _, contract := range spec.Contracts {
			line("\n### Contract: %s", contract.Name)
			line("%s", contract.Text)
		}
		if len(spec.Graph) > 0 {
			line("\n### Workstream graph")
			for _, edge := range spec.Graph {
				line("- %s -> %s (%s)", edge.From, edge.To, edge.Type)
			}
		}
		if slice := spec.Slice; slice != nil {
			line("\n### Slice: %s", slice.Workstream)
			line("spec: %s", slice.Spec)
			line("plan: %s", slice.Plan)
			line("read-only contract criteria: %v", slice.ContractCriteria)
		}
	}
	if len(b.Dependencies) > 0 {
		line("\n## Dependency references")
		line("source: edge-observation.json (record edge-observation)")
		line("Build against the upstream branch or commit while waiting. After a merge, update the dependency reference to its merged commit within the ratified slice, then run checks and final review. Publication and readiness require fresh owner delivery approval.")
		for _, dependency := range b.Dependencies {
			line("- %s (%s): %s; build reference: %s; merged: %t; merged commit: %s", dependency.Upstream, dependency.Type, dependency.Reference, dependency.BuildBranch, dependency.Merged, dependency.Commit)
		}
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
	if len(b.MemberCharters) > 0 {
		line("## Workgroup charters")
		line("Each member's own charter governs its slice, plan and units. The workgroup spec and contracts must satisfy every charter below. Unqualified charter#<n> cites the member's own project. Use the displayed project ID for qualified citations. Conflicts between projects' charter rules require the owner; report them with charter_conflict and cite both rules.")
		for _, m := range b.MemberCharters {
			line("\n### Project %s (member %s)", m.Project, m.Workstream)
			line("source: %s/charter.md (record %s revision %d)", m.Project, m.Document.ID, m.Document.Revision)
			for _, rule := range charter.Parse(m.Document.Content).Rules {
				line("- %s/charter#%d: %s", m.Project, rule.Number, rule.Text)
			}
		}
	}

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
	if len(b.Notices) == 0 && len(b.CharterNotices) == 0 && len(b.CharterEdits) == 0 && len(b.ProjectNotices) == 0 {
		line("No project-wide notices.")
	}
	for _, n := range b.Notices {
		line("- %s (record %s revision %d, workstream %s)", n.Source, n.Record, n.Revision, n.Workstream)
		section, err := envelope.Render(envelope.Section{Name: "owner_response", Text: n.OwnerResponse}, envelope.Section{Name: "returned_answer", Text: n.ReturnedAnswer})
		if err != nil {
			panic(err)
		}
		w.WriteString(section)
	}
	for _, n := range b.CharterNotices {
		line("- %s (record %s revision %d, workstream %s): the owner ratified charter#%d, recorded in charter.md revision %d, from %s revision %d", n.Source, n.Record, n.Revision, n.Workstream, n.Number, n.Charter, n.Ruling, n.RulingRevision)
		section, err := envelope.Render(envelope.Section{Name: "owner_response", Text: n.OwnerResponse}, envelope.Section{Name: "charter_rule", Text: n.Rule})
		if err != nil {
			panic(err)
		}
		w.WriteString(section)
	}
	for _, d := range b.CharterEdits {
		line("- The owner edited charter.md: record %s revision %d. Recheck the current charter before proceeding.", d.ID, d.Revision)
	}
	for _, n := range b.ProjectNotices {
		line("- %s (record %s revision %d, workstream %s, thread %s, turn %s): informational chief-of-staff notice", n.Source, n.Record, n.Revision, n.Scope.Workstream, n.Scope.Thread, n.Scope.Turn)
		section, err := envelope.Render(envelope.Section{Name: "project_notice", Text: n.Text})
		if err != nil {
			panic(err)
		}
		w.WriteString(section)
	}
	if b.MemoryProblem != "" {
		line("\nExternal memory is degraded: %s. Authoritative local context remains available.", b.MemoryProblem)
	}
	for _, data := range b.Memory {
		line("\n## Hearsay context (external evidence)")
		section, err := envelope.Render(envelope.Section{Name: "hearsay_context", Text: string(data)})
		if err != nil {
			panic(err)
		}
		w.WriteString(section)
	}
	return w.String()
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
