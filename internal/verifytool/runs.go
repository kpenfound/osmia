package verifytool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ReviewerRuns returns the verify runs the reviewer of unit made on candidate
// that came to an outcome, in the order they were recorded.
func ReviewerRuns(repo *trace.Repository, stream config.WorkstreamID, unit, candidate string) ([]Operation, error) {
	ops, _, err := Operations(repo, stream)
	if err != nil {
		return nil, err
	}
	var runs []Operation
	for _, op := range ops {
		if op.Scope.Role == "reviewer" && op.Scope.Unit == unit && op.Commit == candidate && op.Outcome != nil {
			runs = append(runs, op)
		}
	}
	return runs, nil
}

// SendBack renders recorded runs in full for the mason a reviewer returned a
// candidate to: every claim with its status, detail, flow tag and evidence
// path, and the status of each flow. Nothing is summarised away. The paths are
// those of the retained files in the mason's view.
func SendBack(runs []Operation) string {
	if len(runs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nThe reviewer ran the project's verification lever on this candidate. These are the results the service recorded, given in full; they are evidence for you to weigh, and a result that is not a pass is not a reason to round it up. The files they reference are in your view at the paths shown, and no run is repeated for you.\n")
	for _, op := range runs {
		data, err := json.MarshalIndent(BuildResult(op), "", "  ")
		if err != nil {
			continue
		}
		args, _ := json.Marshal(op.Args)
		fmt.Fprintf(&b, "\nVerify run %s on commit %s with arguments %s:\n%s\n", op.RunID, op.Commit, args, data)
	}
	return b.String()
}

// ReviewGuidance is added to the system prompt of a unit reviewer and of the
// final reviewer in a project with the verification setting.
const ReviewGuidance = `## Verification
This project has a verification lever, so you have the verify tool. It runs the lever on the service against the exact build you are judging, with an argument list you write, and records the run. A verify result is evidence you judge: it never changes the unit's or the workstream's state, and you decide. Replay what the mason ran if it helps, and also try paths the mason did not, because the mason's own runs are not evidence. Each call starts the app anew, so exploring means calling again with more steps. Inconclusive never counts as a pass. A missing, failed or inconclusive result on a user-visible acceptance item is a finding. The files a run retained are in your view under ` + EvidenceDir + `/<run_id>/ at the paths the result gives.`

// MasonGuidance is added to the system prompt of a mason in a project with the
// verification setting.
const MasonGuidance = `## Verification
This project has a verification lever: a function in the project that drives the running build and returns a report, with a feature map beside it. Your prompt names the feature files that apply to your unit. Keep the lever and the map current in the same change as the behaviour they describe. When you call done, list in invocations the lever argument lists you verified with, in the order you ran them. The service reruns up to 5 of them on your candidate and gives the reviewer those service runs as evidence. Your own runs while you work are for iteration and are not evidence, and a result that is not a pass is never rounded up to one. When a reviewer sends the unit back with verify results, they come to you in full and their files are in your view.`
