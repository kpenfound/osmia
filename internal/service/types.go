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
	// CharterState is reported by status only.
	CharterState *CharterState `json:"charter_state,omitempty"`
}

// CharterState summarizes the charter as last recorded. Ready means it has at
// least one rule, which hand-in requires.
type CharterState struct {
	Ready       bool                 `json:"ready"`
	Rules       int                  `json:"rules"`
	Revision    int                  `json:"revision"`
	Diagnostics []charter.Diagnostic `json:"diagnostics"`
}

// HandInRequest hands work to a project. Paths are absolute paths to the
// design documents being handed in.
type HandInRequest struct {
	Project config.ProjectID `json:"project"`
	Paths   []string         `json:"paths"`
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
