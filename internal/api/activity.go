package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// ActivityKind names the long-running backend operation an activity entry
// reports.
type ActivityKind string

const (
	ActivityArchiveCleanup ActivityKind = "archive_cleanup"
	// ActivityGit covers Git and workspace operations the reconcile
	// controller runs: seal, build, land, rebase, drift and publish.
	ActivityGit       ActivityKind = "git"
	ActivityCheck     ActivityKind = "check"
	ActivityAgentTurn ActivityKind = "turn"
	ActivityRecovery  ActivityKind = "recovery"
)

// ActivityEntry is one backend operation the activity record holds: either
// in flight, or terminally failed and not yet superseded by a fresh start of
// the same operation or expired under the activity registry's failure policy. Progress is nil
// when the operation's completion cannot be measured.
type ActivityEntry struct {
	ID           string              `json:"id"`
	Kind         ActivityKind        `json:"kind"`
	Project      config.ProjectID    `json:"project,omitempty"`
	Workstream   config.WorkstreamID `json:"workstream,omitempty"`
	Phase        string              `json:"phase"`
	Progress     *float64            `json:"progress,omitempty"`
	StartedAt    time.Time           `json:"started_at"`
	LastProgress time.Time           `json:"last_progress"`
	Failure      string              `json:"failure,omitempty"`
}

// ActivityResponse lists the backend operations the activity record holds,
// oldest first.
type ActivityResponse struct {
	Activity []ActivityEntry `json:"activity"`
}
