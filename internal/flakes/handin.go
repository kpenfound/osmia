package flakes

import (
	"fmt"
	"strings"
	"time"
)

// NotesHeading heads the manager's notes, the one part of the hand-in
// the service does not write from evidence.
const NotesHeading = "## Notes from the manager"

// RenderHandIn writes the hand-in for the occurrence of run in the group, as
// Markdown: the evidence its run recorded, the group's earlier history, what
// the workstream is asked to do and, last and separate, the manager's
// notes. Only the notes are free text; everything else is written from the
// record.
func RenderHandIn(g Group, run, notes string) (string, error) {
	o, ok := g.Find(run)
	if !ok {
		return "", ErrUnknownOccurrence
	}
	ev := o.Evidence
	engine := ev.Engine
	if engine == "" {
		engine = "the engine"
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# Find and fix the cause of a flaky check")
	line("")
	line("A check failed and then passed when it was rerun by its own link. The run counted it as a pass, and the factory recorded the flake. This workstream is to find the cause of that flakiness and fix it.")
	line("")
	line("## What was observed")
	line("")
	line("The flakiness showed up in these checks. They are where it was seen, which is not necessarily where the fault is:")
	line("")
	for _, link := range o.Links {
		line("- `%s`", link)
	}
	if others := otherFlaky(ev.Flaky, o.Links); len(others) > 0 {
		line("")
		line("The same run had these other flaky checks, which belong to other flake groups:")
		line("")
		for _, link := range others {
			line("- `%s`", link)
		}
	}
	line("")
	if g.Unresolved {
		line("Flake group: `%s`, a Dagger Cloud check name that matches no known check.", g.Group)
	} else {
		line("Flake group: `%s`.", g.Group)
	}
	line("")
	line("## Where it was recorded")
	line("")
	if o.Source.FinalReview {
		line("- Originating run: the final review of workstream `%s`.", o.Source.Workstream)
	} else {
		line("- Originating run: unit `%s` of workstream `%s`.", o.Source.Unit, o.Source.Workstream)
	}
	line("- Run record: `%s` in the trace of workstream `%s`.", o.RunPath(), o.Source.Workstream)
	line("- Recorded: %s.", o.RecordedAt.UTC().Format(time.RFC3339))
	line("- Engine: %s.", engineLine(ev.Engine))
	line("")
	line("## Evidence")
	line("")
	where := whereToRead(o)
	renderOutput(&b, o, engine, where)
	renderDiagnostics(&b, ev, engine, where)
	renderCoRun(&b, ev, engine, where)
	if len(ev.Timings) > 0 {
		line("### Timings")
		line("")
		for _, t := range ev.Timings {
			line("- `%s`: %s", t.Check, t.Duration.Round(time.Millisecond))
		}
		line("")
	}
	renderHistory(&b, g, run)

	line("## What this workstream is for")
	line("")
	line("Find the cause of the flakiness these links show and fix it. The cause may be in the flaky test, in another test, a shared fixture or helper, production code, the project's Dagger check module (for example its parallelism or resources), or anywhere else it lives. Follow the evidence above, including the checks that ran alongside and how many ran at once, since resource pressure and interference between tests show up only there. Repeated, stress or race runs are welcome as evidence; they are not required.")
	line("")
	line("## Outcome")
	line("")
	line("The work is done when the cause it identified is addressed, and the reasoning and the evidence for that cause are recorded in the workstream. The record of a fix says what the cause is, why the evidence points to it and how the change addresses it. Which files change follows from the cause.")
	line("")
	line("## Reviewing")
	line("")
	line("Unit and final reviewers: do not raise a finding only because the named test is unchanged, or because the flake could not be reproduced on demand. Judge whether the stated cause is plausible and supported by the evidence, and whether the change addresses it.")
	line("")
	line("## When the cause is outside the repository")
	line("")
	line("If the cause lies outside this repository, the existing \"no change needed\" escalation to the owner applies. Give the evidence for that cause when escalating.")
	line("")
	line("%s", NotesHeading)
	line("")
	if notes = strings.TrimSpace(notes); notes == "" {
		line("None.")
	} else {
		line("%s", notes)
	}
	return b.String(), nil
}

func engineLine(engine string) string {
	switch engine {
	case EngineLocal:
		return "`local`, the factory's own Dagger engine"
	case EngineCloud:
		return "`cloud`, Dagger Cloud"
	case "":
		return "not recorded"
	}
	return "`" + engine + "`"
}

func otherFlaky(all, links []string) []string {
	var out []string
	for _, l := range all {
		found := false
		for _, k := range links {
			found = found || k == l
		}
		if !found {
			out = append(out, l)
		}
	}
	return out
}

func whereToRead(o Occurrence) string {
	var refs []string
	for _, t := range o.Evidence.Traces {
		refs = append(refs, traceCommand(t))
	}
	if len(refs) > 0 {
		return "read " + strings.Join(refs, ", ")
	}
	return fmt.Sprintf("read the run record `%s`", o.RunPath())
}

func traceCommand(t Trace) string {
	text := ""
	if strings.HasPrefix(t.Ref, "http://") || strings.HasPrefix(t.Ref, "https://") {
		text = "`" + t.Ref + "`"
	} else {
		text = "`dagger trace " + t.Ref + "`"
	}
	var label []string
	if t.Check != "" {
		label = append(label, t.Check)
	}
	if t.Role != "" {
		label = append(label, t.Role)
	}
	if len(label) > 0 {
		text += " (" + strings.Join(label, ", ") + ")"
	}
	return text
}

func renderOutput(b *strings.Builder, o Occurrence, engine, where string) {
	ev := o.Evidence
	fmt.Fprintf(b, "### Captured output\n\n")
	if ev.OutputPath != "" {
		fmt.Fprintf(b, "The whole output is recorded at `%s` in the trace of workstream `%s`.\n\n", ev.OutputPath, o.Source.Workstream)
	}
	switch {
	case strings.TrimSpace(ev.Output) != "":
		b.WriteString(fenced(ev.Output))
		b.WriteString("\n")
	case ev.OutputPath == "":
		fmt.Fprintf(b, "The captured output is not reported by %s; %s.\n\n", engine, where)
	}
}

func renderDiagnostics(b *strings.Builder, ev Evidence, engine, where string) {
	fmt.Fprintf(b, "### Diagnostics of the failures that passed on rerun\n\n")
	if len(ev.Diagnostics) == 0 {
		fmt.Fprintf(b, "The `[passed on rerun]` diagnostics are not reported by %s; %s.\n\n", engine, where)
		return
	}
	for _, d := range ev.Diagnostics {
		b.WriteString(fenced(d))
		b.WriteString("\n")
	}
}

func renderCoRun(b *strings.Builder, ev Evidence, engine, where string) {
	fmt.Fprintf(b, "### Other checks in the same invocation\n\n")
	if len(ev.CoRun) == 0 {
		fmt.Fprintf(b, "The other checks that ran in the same invocation are not reported by %s; %s.\n\n", engine, where)
	} else {
		for _, c := range ev.CoRun {
			fmt.Fprintf(b, "- `%s`\n", c)
		}
		b.WriteString("\n")
	}
	if ev.Parallelism > 0 {
		fmt.Fprintf(b, "Parallelism: %d checks at once.\n\n", ev.Parallelism)
	} else {
		fmt.Fprintf(b, "The parallelism of the invocation is not reported by %s; %s.\n\n", engine, where)
	}
}

func renderHistory(b *strings.Builder, g Group, run string) {
	history := g.History(run)
	if len(history) == 0 && len(g.Closed) == 0 {
		return
	}
	fmt.Fprintf(b, "## Earlier history of this flake group\n\n")
	for _, c := range g.Closed {
		fmt.Fprintf(b, "- %s: the flake-fix workstream `%s` of this group was delivered or abandoned.\n", c.At.UTC().Format(time.RFC3339), c.Workstream)
	}
	for _, h := range history {
		origin := fmt.Sprintf("unit `%s` of workstream `%s`", h.Source.Unit, h.Source.Workstream)
		if h.Source.FinalReview {
			origin = fmt.Sprintf("the final review of workstream `%s`", h.Source.Workstream)
		}
		fmt.Fprintf(b, "- %s: flaky %s in %s, run record `%s`; %s.\n", h.RecordedAt.UTC().Format(time.RFC3339), codeList(h.Links), origin, h.RunPath(), describeDecision(h.Decision))
	}
	b.WriteString("\n")
}

func describeDecision(d Decision) string {
	switch d.State {
	case Opened:
		return fmt.Sprintf("it opened flake-fix workstream `%s`", d.Workstream)
	case Joined:
		return fmt.Sprintf("it joined flake-fix workstream `%s`", d.Workstream)
	case Declined:
		return "it was declined: " + d.Reason
	case Opening:
		return fmt.Sprintf("a flake-fix workstream `%s` was being opened for it", d.Workstream)
	}
	return "it had no decision yet"
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "`" + item + "`"
	}
	return strings.Join(quoted, ", ")
}

func fenced(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + strings.TrimRight(text, "\n") + "\n" + fence + "\n"
}
