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

// The workstream documents of the climbing loop: what the running attempt is
// doing, the steering notes the chief of staff left, the budget extensions the
// owner granted, and one journal and one stop record per attempt.
const (
	ClimbRunningID    = "climb-running"
	ClimbSteeringPath = "climb/steering.json"
	ClimbSteeringID   = "climb-steering"
	ClimbBudgetPath   = "climb/budget.json"
	ClimbBudgetID     = "climb-budget"
	ClimbJournalDir   = "climb/journal"
	ClimbStopsDir     = "climb/stops"
)

// ClimbJournalPath is the workstream document path of the journal of attempt
// n, and ClimbJournalID its document identity.
func ClimbJournalPath(n int) string { return fmt.Sprintf("%s/%d.json", ClimbJournalDir, n) }

// ClimbJournalID is the document identity of the journal of attempt n.
func ClimbJournalID(n int) string { return fmt.Sprintf("climb-journal-%d", n) }

// ClimbStopPath is the workstream document path of the stop record the
// climb.stop exit after attempt n leaves, and ClimbStopID its identity.
func ClimbStopPath(n int) string { return fmt.Sprintf("%s/%d.json", ClimbStopsDir, n) }

// ClimbStopID is the document identity of the stop record after attempt n.
func ClimbStopID(n int) string { return fmt.Sprintf("climb-stop-%d", n) }

// ClimbAttemptSubject is the workflow subject that holds how far attempt n has
// got: started, captured, measured, reviewed, promoting, promoted, recorded
// and cleaned.
func ClimbAttemptSubject(n int) string { return fmt.Sprintf("climb-attempt-%d", n) }

func numbered(name, suffix string) bool {
	n, ok := strings.CutSuffix(name, suffix)
	return ok && shedRound.MatchString("round-"+n)
}

func climbRecordPath(parts []string) bool {
	if len(parts) == 2 && parts[0] == "climb" {
		switch parts[1] {
		case "evaluator.json", "calibration.json", "approval.json", "running.json", "steering.json", "budget.json":
			return true
		}
		return false
	}
	if len(parts) == 3 && parts[0] == "climb" && (parts[1] == "journal" || parts[1] == "stops") {
		return numbered(parts[2], ".json")
	}
	return false
}
