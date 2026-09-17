// Package service owns the M1 stores and their local HTTP boundary.
package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

const Prefix = "/v1"

type Code string

const (
	Malformed       Code = "malformed_input"
	Validation      Code = "validation"
	Conflict        Code = "conflict"
	Unsupported     Code = "unsupported"
	RestartRequired Code = "restart_required"
	Unavailable     Code = "unavailable"
	Internal        Code = "internal"
	NoProject       Code = "no_project"
	ProjectActive   Code = "project_active"
	NotFound        Code = "not_found"
	CharterEmpty    Code = "charter_empty"
)

type APIError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return string(e.Code) + ": " + e.Message }

type ErrorResponse struct {
	Error APIError `json:"error"`
}
type Identity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}
type HealthResponse struct {
	Ready      bool     `json:"ready"`
	Service    string   `json:"service"`
	APIVersion int      `json:"api_version"`
	Build      Identity `json:"build"`
}
type Diagnostic struct {
	Field   string `json:"field"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// ConfigResponse contains validated fields only, never raw configuration text.
// Project is null until a project is registered.
type ConfigResponse struct {
	Root        string         `json:"root"`
	Digest      string         `json:"digest"`
	Effective   *config.Config `json:"effective"`
	Project     *ProjectView   `json:"project"`
	Diagnostics []Diagnostic   `json:"diagnostics"`
}

// ProjectView names a registered project, its trace and its charter file.
type ProjectView struct {
	ID         config.ProjectID `json:"id"`
	Name       string           `json:"name"`
	Upstream   string           `json:"upstream"`
	Fork       string           `json:"fork"`
	Clone      string           `json:"clone"`
	BaseBranch string           `json:"base_branch"`
	Trace      string           `json:"trace"`
	Charter    string           `json:"charter"`
	// CharterState and Extraction are reported by status only.
	CharterState *CharterState    `json:"charter_state,omitempty"`
	Extraction   *ExtractionState `json:"extraction,omitempty"`
}

// ExtractionState is the latest knowledge-base extraction of a project: its
// number, whether it is pending, running, succeeded or failed, the time of its
// last recorded activity and, when it failed or is waiting to retry, why.
type ExtractionState struct {
	Extraction int       `json:"extraction"`
	State      string    `json:"state"`
	At         time.Time `json:"at"`
	Reason     string    `json:"reason,omitempty"`
}

// ProjectExtractRequest starts a new extraction pass of the active project.
type ProjectExtractRequest struct {
	Project config.ProjectID `json:"project"`
}
type ExtractionResponse struct {
	Project    ProjectView     `json:"project"`
	Extraction ExtractionState `json:"extraction"`
}

// CharterState summarizes the charter as last recorded. Ready means it has at
// least one rule, which hand-in requires.
type CharterState struct {
	Ready       bool                 `json:"ready"`
	Rules       int                  `json:"rules"`
	Revision    int                  `json:"revision"`
	Diagnostics []charter.Diagnostic `json:"diagnostics"`
}

// HandInRequest hands work to a project. Exactly one of Path (an absolute
// path to a file), URL (a GitHub issue URL) and Stdin (the input itself) is
// set. Key identifies the request: a retry with the same key returns the same
// workstream.
type HandInRequest struct {
	Project config.ProjectID `json:"project"`
	Key     string           `json:"key"`
	Path    string           `json:"path,omitempty"`
	URL     string           `json:"url,omitempty"`
	Stdin   *string          `json:"stdin,omitempty"`
}

// HandInResponse names the workstream a hand-in created. Handed is the path
// of the copied input; Source is where it came from, as recorded.
type HandInResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Handed     string              `json:"handed"`
	Source     string              `json:"source"`
}

// ProjectAddRequest registers a project. Clone is an absolute path to an
// existing local Git repository; base_branch defaults to main.
type ProjectAddRequest struct {
	Name       string `json:"name"`
	Upstream   string `json:"upstream"`
	Fork       string `json:"fork"`
	Clone      string `json:"clone"`
	BaseBranch string `json:"base_branch,omitempty"`
}
type ProjectRemoveRequest struct {
	Project config.ProjectID `json:"project"`
}
type ProjectResponse struct {
	Project  ProjectView `json:"project"`
	NextStep string      `json:"next_step"`
}
type RuntimeResponse struct {
	Effective runtime.State `json:"effective"`
	// Projects lists each active project's context mode.
	Projects    []ProjectRuntime `json:"projects"`
	Diagnostics []Diagnostic     `json:"diagnostics"`
}

// ProjectRuntime reports where a project's turn context comes from. "file"
// is the supported local mode, not a degraded one.
type ProjectRuntime struct {
	Project     config.ProjectID `json:"project"`
	ContextMode bundle.Mode      `json:"context_mode"`
}
type PauseRequest = runtime.Pause
type ClearPauseRequest = runtime.Target
type PriorityRequest = runtime.Priority
type ClearPriorityRequest struct {
	Project config.ProjectID `json:"project"`
}
type ProfileRequest struct {
	Role    string `json:"role"`
	Profile string `json:"profile"`
}
type ClearProfileRequest struct {
	Role string `json:"role"`
}
type MutationResponse struct {
	Applied bool `json:"applied"`
}

// StatusResponse lists every workstream of the active project. It is empty
// without an active project or its trace, and when the trace cannot be read,
// which a diagnostic reports.
type StatusResponse struct {
	Workstreams []WorkstreamStatus `json:"workstreams"`
	Diagnostics []Diagnostic       `json:"diagnostics"`
}

// WorkstreamStatus is the chief of staff's status for one workstream, next to
// the facts the service owns. State is null until a feature state is
// recorded; Status is null until the chief of staff writes one.
type WorkstreamStatus struct {
	Workstream    config.WorkstreamID `json:"workstream"`
	Project       config.ProjectID    `json:"project"`
	State         *string             `json:"state"`
	OpenQuestions int                 `json:"open_questions"`
	ContextMode   bundle.Mode         `json:"context_mode"`
	Status        *StatusView         `json:"status"`
}

// StatusView is one status revision as the chief of staff wrote it.
type StatusView struct {
	Goal      string    `json:"goal"`
	Attention string    `json:"attention"`
	Note      string    `json:"note"`
	Agents    []string  `json:"agents"`
	Revision  int       `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SendRequest is an owner message to a workstream's chief of staff.
type SendRequest struct {
	Text string `json:"text"`
}

// TurnState is the execution state of the turn a conversation entry belongs to.
type TurnState string

const (
	TurnQueued  TurnState = "queued"
	TurnRunning TurnState = "running"
	TurnDone    TurnState = "done"
	TurnFailed  TurnState = "failed"
)

// ConversationResponse lists a workstream's conversation with its chief of
// staff, oldest first.
type ConversationResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Entries    []ConversationEntry `json:"entries"`
}

// ConversationEntry is an owner message (kind "message") or the chief of
// staff's final response to it (kind "response"). Both carry the state of the
// turn that answers the message, and At is when the message was accepted or
// the response captured.
type ConversationEntry struct {
	Turn  string    `json:"turn"`
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
	State TurnState `json:"state"`
}
