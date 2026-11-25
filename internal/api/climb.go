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
// Commit pins the evaluator commit the card showed, which is the card's answer
// body; an approval of a commit the climb branch no longer holds is refused as
// a conflict.
type ClimbApprovalRequest struct {
	Decision string `json:"decision" description:"approve is the one decision the card offers."`
	Commit   string `json:"commit" description:"The evaluator commit the card showed."`
}

// ClimbApprovalResponse reports an approved evaluator: the protected paths are
// frozen as they were at the evaluator commit and climbing starts from the
// baseline.
type ClimbApprovalResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Evaluator  string              `json:"evaluator"`
	Commit     string              `json:"commit"`
	Baseline   float64             `json:"baseline"`
	Noise      float64             `json:"noise"`
	Protected  []string            `json:"protected"`
	At         time.Time           `json:"at"`
}
