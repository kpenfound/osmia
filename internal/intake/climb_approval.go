package intake

import (
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

const maxApprovalDiff = 24 << 10

// ClimbApprovalEntry is the owner's approval card of a Hill Climb workstream's
// calibrated evaluator: the evaluator diff, the protected paths the approval
// freezes, the baseline and the spread. It is open while the climb waits for
// the approval and never consults Jev: it belongs to no gate, and only the
// owner's explicit answer to it starts climbing.
func (s *Controller) ClimbApprovalEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	if w.State != BuildingState || w.WorkKind != kinds.HillClimb || w.Subjects[climb.InnerSubject].Value != climb.InnerAwaitingApproval {
		return InboxEntry{}, false, nil
	}
	rec, _, found, err := climb.ReadCalibration(repository, w.Workstream)
	if err != nil || !found || rec.Calibration.Verdict != climb.Calibrated {
		return InboxEntry{}, false, err
	}
	cal := rec.Calibration
	unit := cal.Units[rec.Primary]
	e := Decision(api.InboxClimbApproval, w.Workstream, rec.At, "/climb/"+string(w.Workstream)+"/approval", map[string]any{"commit": rec.Commit})
	var q strings.Builder
	fmt.Fprintf(&q, "Approve the evaluator %q for this climb? It was calibrated on commit %s: the baseline is %v %s and the run-to-run spread over %d runs is %v %s, below the minimum improvement.\n\n", rec.Evaluator, rec.Commit, cal.Baseline, unit, len(cal.Measurement.Runs), cal.Spread, unit)
	fmt.Fprintf(&q, "Approving freezes these protected paths as they are at that commit; every measurement restores them and an attempt that changes one is refused: %s.\n\n", strings.Join(rec.Protected, ", "))
	if strings.TrimSpace(rec.Diff) == "" {
		q.WriteString("Evaluator diff: none. The project already had an evaluator matching the climb spec, and it is reused.")
	} else {
		fmt.Fprintf(&q, "Evaluator diff:\n%s", limitText(rec.Diff, maxApprovalDiff))
	}
	e.Question = q.String()
	e.Blocked = "The climb: no attempt starts until you approve."
	e.Options = []string{"approve"}
	return e, true, nil
}
