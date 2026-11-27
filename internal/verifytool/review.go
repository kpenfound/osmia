package verifytool

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/verification"
)

const maxDetailRunes = 1000

// ReviewInput is what a unit review bundle shows of verification.
type ReviewInput struct {
	// Declared is how many invocations the mason declared.
	Declared int
	// Reruns are the service's reruns of them, whether they ran, were skipped
	// or were refused.
	Reruns []Operation
	// Runs are the service-recorded runs on the candidate that came to an
	// outcome: its reruns and its reviewer's verify runs.
	Runs []Operation
	// Features are the features the unit's diff touches.
	Features []verification.Feature
	// Listed are the flows the lever lists, nil when the list is unknown.
	Listed []string
	// Notes are the structure-check notes of the feature map.
	Notes []verification.Note
	// Flags are the lever and map edits the diff holds.
	Flags []string
}

// RenderReview renders the verification sections of a unit review bundle:
// the service's reruns, kept apart from the mason's own report; the features
// the diff touches with the status service-recorded runs gave each flow, or
// that none covered it; and the notes and flags for the reviewer. Nothing in
// it moves a unit: the reviewer decides, and inconclusive is never shown as a
// pass.
func RenderReview(in ReviewInput) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("## Verification: service-recorded reruns")
	line("The service itself reran the lever invocations the mason declared, on this exact candidate. These runs are the service's evidence. The mason's report text and claims are the mason's own and are not evidence. Inconclusive is never a pass. The files each run kept are in your view under %s/<run_id>/.", EvidenceDir)
	var ran, skipped, refused int
	for _, op := range in.Reruns {
		switch op.Rerun.Disposition {
		case RerunSkipped:
			skipped++
		case RerunRefused:
			refused++
		default:
			ran++
		}
	}
	line("Declared invocations: %d. Rerun: %d. Skipped: %d. Refused: %d.", in.Declared, ran, skipped, refused)
	switch {
	case in.Declared == 0:
		line("The mason declared no invocations, so the service reran nothing.")
	case len(in.Reruns) < in.Declared:
		line("The service recorded %d of the %d declared invocations: the rest were not reached, so no service run stands behind them.", len(in.Reruns), in.Declared)
	}
	for _, op := range in.Reruns {
		line("")
		renderRerun(&b, op)
	}
	line("")
	line("## Verification: features the diff touches")
	if len(in.Features) == 0 {
		line("The diff touches no file that a feature's paths globs match.")
	}
	var used []Operation
	var coverage []verification.Coverage
	for _, op := range in.Runs {
		if op.Outcome != nil && op.Outcome.Limitation == "" {
			used = append(used, op)
			coverage = append(coverage, verification.Attribute(op.Outcome.Result, in.Listed))
		}
	}
	combined := map[string]verification.FlowStatus{}
	for _, f := range verification.Combine(coverage...) {
		combined[f.Flow] = f
	}
	for _, f := range in.Features {
		line("- %s (%s)", f.Name, f.File)
		if len(f.Flows) == 0 {
			line("  This feature names no flows.")
		}
		for _, flow := range f.Flows {
			status, covered := combined[flow]
			if !covered {
				line("  - flow %s: no service-recorded run covered it", flow)
				continue
			}
			line("  - flow %s: %s (%s)", flow, status.Status, flowRuns(used, coverage, flow))
		}
	}
	renderUncovered(&b, used, coverage, combined)
	if len(in.Notes) != 0 || len(in.Flags) != 0 {
		line("")
		line("## Verification: notes for the reviewer")
		line("These notes and flags block nothing and change no state. They tell you where to look closely.")
		for _, flag := range in.Flags {
			line("- Flag: %s", flag)
		}
		for _, note := range in.Notes {
			line("- Structure check: %s", note)
		}
	}
	return b.String()
}

func renderRerun(b *strings.Builder, op Operation) {
	rerun := op.Rerun
	switch rerun.Disposition {
	case RerunSkipped, RerunRefused:
		fmt.Fprintf(b, "### Declared invocation %d: %s, no run\narguments: %s\nreason: %s\n", rerun.Index, rerun.Disposition, quoted(op.Args), rerun.Reason)
		return
	}
	fmt.Fprintf(b, "### Declared invocation %d: run %s on commit %s\narguments: %s\n", rerun.Index, op.RunID, op.Commit, quoted(op.Args))
	o := op.Outcome
	switch {
	case o == nil:
		b.WriteString("This run recorded no outcome yet.\n")
	case o.Limitation != "":
		fmt.Fprintf(b, "no run result: %s\nThis is not a pass.\n", o.Limitation)
	default:
		renderResult(b, op, *o)
	}
}

func renderResult(b *strings.Builder, op Operation, o Outcome) {
	res := o.Result
	status := res.Status()
	if res.Rejected != "" {
		fmt.Fprintf(b, "status: %s (the report was rejected: %s)\nA rejected report covers no flow.\n", status, res.Rejected)
	} else {
		fmt.Fprintf(b, "status: %s\n", status)
	}
	for _, c := range res.Claims {
		flow := "no flow tag"
		if c.Flow != "" {
			flow = "flow " + c.Flow
		}
		fmt.Fprintf(b, "- [%s] %s (%s): %s\n", c.Status, c.Name, flow, oneLine(c.Detail))
		for _, p := range c.Evidence {
			fmt.Fprintf(b, "  evidence: %s\n", Ref(op.RunID, p))
		}
	}
	if o.Diagnostics != "" {
		fmt.Fprintf(b, "diagnostics: %s\n", oneLine(o.Diagnostics))
	}
}

func flowRuns(runs []Operation, coverage []verification.Coverage, flow string) string {
	var parts []string
	for i, cov := range coverage {
		for _, f := range cov.Flows {
			if f.Flow == flow {
				parts = append(parts, fmt.Sprintf("run %s: %s", runs[i].RunID, f.Status))
			}
		}
	}
	return strings.Join(parts, "; ")
}

func renderUncovered(b *strings.Builder, used []Operation, coverage []verification.Coverage, combined map[string]verification.FlowStatus) {
	var untagged, rejected, unlisted []string
	for i, cov := range coverage {
		run := used[i].RunID
		if cov.Rejected != "" {
			rejected = append(rejected, fmt.Sprintf("- run %s: inconclusive, covers no flow: %s", run, cov.Rejected))
		}
		for _, c := range cov.Untagged {
			untagged = append(untagged, fmt.Sprintf("- run %s: [%s] %s: %s", run, c.Status, c.Name, oneLine(c.Detail)))
		}
	}
	for _, f := range combined {
		if f.Unlisted {
			unlisted = append(unlisted, fmt.Sprintf("- %s: %s", f.Flow, f.Status))
		}
	}
	slices.Sort(unlisted)
	for _, section := range []struct {
		title string
		items []string
	}{
		{"Untagged claims: they count toward their run's status and cover no flow", untagged},
		{"Flow tags the lever's list does not name (unlisted flows)", unlisted},
		{"Rejected reports", rejected},
	} {
		if len(section.items) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n%s:\n%s\n", section.title, strings.Join(section.items, "\n"))
	}
}

func quoted(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = fmt.Sprintf("%q", a)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxDetailRunes {
		return string(r[:maxDetailRunes]) + "…"
	}
	return s
}
