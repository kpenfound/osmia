package trace

import (
	"fmt"
	"strings"
)

// ClimbAttemptsDir is the workstream folder that holds one document per
// Hill Climb attempt.
const ClimbAttemptsDir = "climb/attempts"

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
