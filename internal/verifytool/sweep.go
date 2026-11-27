package verifytool

import (
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/verification"
)

// SweepInput is what the final reviewer's regression sweep is made of.
type SweepInput struct {
	// Dir is the feature-map directory.
	Dir string
	// Commit is the feature-branch head the map was read from.
	Commit string
	// Features are the map's features in sweep order.
	Features []verification.Feature
	// Notes are the structure-check notes of the map.
	Notes []verification.Note
}

// SweepFlows returns the union of the flows the features name, once each, in
// the order of the features, with the features that name each of them.
func SweepFlows(features []verification.Feature) (flows []string, named map[string][]string) {
	named = map[string][]string{}
	for _, f := range features {
		for _, flow := range f.Flows {
			if len(named[flow]) == 0 {
				flows = append(flows, flow)
			}
			named[flow] = append(named[flow], f.Name)
		}
	}
	return flows, named
}

// RenderSweep renders the final reviewer's regression sweep: every flow the
// feature map names, once each, and the instruction to run them with verify on
// the feature-branch head. The notes block nothing, and a sweep result is
// evidence the reviewer judges: it never changes the workstream's state.
func RenderSweep(in SweepInput) string {
	flows, named := SweepFlows(in.Features)
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("## Verification: regression sweep")
	line("This project has a verification lever, and the feature map in %s on the feature-branch head %s names these flows. They are the regression sweep for the assembled feature: run each one with verify on the feature-branch head, using the lever argument list the project's verification documentation and the feature files give for it, and judge the results as evidence. Inconclusive is never a pass. A flow you did not run, or one that failed or came back inconclusive, is a finding when it covers a user-visible acceptance item. Nothing here changes the workstream's state on its own.", in.Dir, in.Commit)
	if len(flows) == 0 {
		line("")
		line("The feature map names no flows, so there is nothing to sweep.")
	}
	for i, flow := range flows {
		line("%d. %s (%s)", i+1, flow, strings.Join(named[flow], ", "))
	}
	if len(in.Notes) != 0 {
		line("")
		line("Structure check of the map. These notes block nothing; they tell you where the map or the lever may be out of date:")
		for _, note := range in.Notes {
			line("- %s", note)
		}
	}
	return b.String()
}
