package trace

import (
	"fmt"
	"strings"
)

// ClimbAttemptsDir is the workstream folder that holds one document per
// Hill Climb attempt.
const ClimbAttemptsDir = "climb/attempts"

// ClimbRunningPath is the workstream document that says which Hill Climb
// attempt is in flight and in which phase.
const ClimbRunningPath = "climb/running.json"

// ClimbAttemptPath is the workstream document path of attempt n.
func ClimbAttemptPath(n int) string { return fmt.Sprintf("%s/%d.json", ClimbAttemptsDir, n) }

// ClimbAttemptID is the document identity of attempt n.
func ClimbAttemptID(n int) string { return fmt.Sprintf("climb-attempt-%d", n) }

func climbAttemptPath(parts []string) bool {
	if len(parts) == 2 && parts[0] == "climb" && parts[1] == "running.json" {
		return true
	}
	if len(parts) != 3 || parts[0] != "climb" || parts[1] != "attempts" {
		return false
	}
	n, ok := strings.CutSuffix(parts[2], ".json")
	return ok && shedRound.MatchString("round-"+n)
}

// ClimbSubject is the workflow subject of a Hill Climb workstream's inner
// state while it is building.
const ClimbSubject = "climb"

// The workstream documents of the climb's inner machine before climbing:
// what the evaluator step decided, what calibration measured and what the
// owner approved.
const (
	ClimbEvaluatorPath   = "climb/evaluator.json"
	ClimbEvaluatorID     = "climb-evaluator"
	ClimbCalibrationPath = "climb/calibration.json"
	ClimbCalibrationID   = "climb-calibration"
	ClimbApprovalPath    = "climb/approval.json"
	ClimbApprovalID      = "climb-approval"
)

func climbRecordPath(parts []string) bool {
	if len(parts) != 2 || parts[0] != "climb" {
		return false
	}
	switch parts[1] {
	case "evaluator.json", "calibration.json", "approval.json":
		return true
	}
	return false
}
