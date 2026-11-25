package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// InboxClimbApproval asks the owner to approve a Hill Climb workstream's
// calibrated evaluator. Only the owner's own decision on it starts climbing; Jev
// never decides it.
const InboxClimbApproval = "climb-approval"

// ClimbApprovalRequest is the owner's decision on the evaluator approval card.
// Commit and SpecRevision pin the evaluator commit and the climb spec revision
// the card showed, which are the card's answer body; an approval of a commit
// the climb branch no longer holds, or of a card an amendment of the climb spec
// has retired, is refused as a conflict.
type ClimbApprovalRequest struct {
	Decision     string `json:"decision" description:"approve is the one decision the card offers."`
	Commit       string `json:"commit" description:"The evaluator commit the card showed."`
	SpecRevision int    `json:"spec_revision" description:"The climb spec revision the evaluator was calibrated under, as the card showed it. An amendment that changes a field the calibration depends on retires the card, and a retired card's answer is refused."`
}

// ClimbApprovalResponse reports an approved evaluator: the protected paths are
// frozen as they were at the evaluator commit and climbing starts from the
// baseline.
type ClimbApprovalResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Evaluator  string              `json:"evaluator"`
	Commit     string              `json:"commit"`
	// SpecRevision is the climb spec revision the approved calibration was
	// made under.
	SpecRevision int       `json:"spec_revision"`
	Baseline     float64   `json:"baseline"`
	Noise        float64   `json:"noise"`
	Protected    []string  `json:"protected"`
	At           time.Time `json:"at"`
}

// ClimbConcludeRequest is the owner's decision to conclude a Hill Climb
// workstream now, whatever its stop rules say.
type ClimbConcludeRequest struct {
	Note string `json:"note,omitempty" description:"Why the climb is concluded, which the chief of staff reads."`
}

// ClimbConcludeResponse reports a climb that moved to concluding: no attempt
// starts again, an attempt in flight is discarded, and the branch goes on to its
// rebase and delivery.
type ClimbConcludeResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
}

// ClimbExtendRequest is the owner's extension of a climbing Hill Climb
// workstream's budget, as a pinned owner move: more attempts, more cost, more
// wall-clock time, or any of them. An extension is how a climb goes on past a
// stop rule that is a hard budget.
type ClimbExtendRequest struct {
	Attempts  int     `json:"attempts,omitempty" description:"Further attempts the climb may make."`
	CostUSD   float64 `json:"cost_usd,omitempty" description:"Further dollars the climb may spend."`
	WallClock string  `json:"wall_clock,omitempty" description:"Further time the climb may run, as a duration such as 2h."`
	Note      string  `json:"note,omitempty" description:"Why the budget is extended."`
}

// ClimbExtendResponse reports the extension that was recorded.
type ClimbExtendResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	// Extension is the number of the extension among those granted.
	Extension int     `json:"extension"`
	Attempts  int     `json:"attempts,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	WallClock string  `json:"wall_clock,omitempty"`
}
