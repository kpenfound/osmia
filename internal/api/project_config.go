package api

import "github.com/kpenfound/osmia/internal/config"

// ProjectConfigSetting is one setting of a project's stored settings. Value is
// what the stored revision gives it, or would give it: its default, or for
// capacity.per_workstream the top-level value. Default says the revision leaves
// the setting out. Effective is the value in the loaded configuration, and
// Pending says the stored value is not yet applied. When the stored settings do
// not load, only Name and Effective are present.
type ProjectConfigSetting struct {
	Name      string `json:"name" description:"Setting name: name, upstream, fork, clone, base_branch, landing, delivery, upstream_rebase, pull_request_poll, checks_timeout, capacity.per_workstream, capacity.git_operations, hearsay_scope, hearsay_declare_repo, flake_fixes or hearsay_entities."`
	Value     any    `json:"value,omitempty" description:"The stored value, or the value it would take: a string, an integer, a boolean for hearsay_declare_repo and flake_fixes, or for hearsay_entities an object of local entity ID to Hearsay entity id."`
	Default   *bool  `json:"default,omitempty" description:"True when the stored settings leave the setting out and Value is its default or inherited value."`
	Effective any    `json:"effective" description:"The value in the loaded configuration."`
	Pending   bool   `json:"pending" description:"True when Value differs from Effective: the stored settings hold an edit that is not yet applied."`
}

// ProjectConfigError is the error a project's stored settings fail to load
// with.
type ProjectConfigError struct {
	Field   string `json:"field,omitempty" description:"The setting at fault, empty when the whole revision is."`
	Message string `json:"message" description:"The loader's error for the stored settings."`
}

// ProjectConfigResponse is the editable snapshot of one project's stored
// settings: the settings, the markers and the pin all describe the same stored
// revision. When the revision does not load, Error says why, Settings hold the
// values in effect and Pin is empty, so no save is possible.
type ProjectConfigResponse struct {
	Project  config.ProjectID       `json:"project"`
	Revision int                    `json:"revision" description:"The number of the stored revision the settings come from."`
	Pin      string                 `json:"pin,omitempty" description:"Names the stored revision; send it with a save."`
	Error    *ProjectConfigError    `json:"error,omitempty"`
	Settings []ProjectConfigSetting `json:"settings"`
}

// ProjectConfigValues holds the settings a save sets; an omitted setting is
// not changed. An empty string for name, fork or hearsay_scope
// clears it.
type ProjectConfigValues struct {
	Name                  *string           `json:"name,omitempty"`
	Upstream              *string           `json:"upstream,omitempty"`
	Fork                  *string           `json:"fork,omitempty"`
	Clone                 *string           `json:"clone,omitempty"`
	BaseBranch            *string           `json:"base_branch,omitempty"`
	Landing               *string           `json:"landing,omitempty"`
	Delivery              *string           `json:"delivery,omitempty" description:"pull-request (the default) delivers workstreams as pull requests; jev opts the project in to landing on its base branch and is refused on a project that delivers through a fork."`
	UpstreamRebase        *string           `json:"upstream_rebase,omitempty"`
	PullRequestPoll       *string           `json:"pull_request_poll,omitempty"`
	ChecksTimeout         *string           `json:"checks_timeout,omitempty"`
	CapacityPerWorkstream *int              `json:"capacity.per_workstream,omitempty"`
	CapacityGitOperations *int              `json:"capacity.git_operations,omitempty"`
	HearsayScope          *string           `json:"hearsay_scope,omitempty" description:"A Hearsay entity id, such as work:<source>:<project>, that replaces the project's default read entity."`
	HearsayDeclareRepo    *bool             `json:"hearsay_declare_repo,omitempty" description:"True posts the project to Hearsay with its GitHub repo declared, which makes its records visible to that repository's readers. Default false."`
	FlakeFixes            *bool             `json:"flake_fixes,omitempty" description:"True turns flake-fix workstreams on for the project: a flaky check is recorded and the chief of staff may open a workstream to fix its cause. Those workstreams' pull requests go out under the owner's name. Default false."`
	HearsayEntities       map[string]string `json:"hearsay_entities,omitempty" description:"Entries to set, by local entity ID, each a Hearsay entity id."`
}

// ProjectConfigEdit is a save of one project's stored settings: the pin of the
// snapshot the owner edited and the changes. Clear names settings that are
// removed from the revision so the default or inherited value applies again,
// and ClearHearsayEntities names hearsay_entities entries to remove.
type ProjectConfigEdit struct {
	Pin                  string              `json:"pin"`
	Set                  ProjectConfigValues `json:"set"`
	Clear                []string            `json:"clear,omitempty"`
	ClearHearsayEntities []string            `json:"clear_hearsay_entities,omitempty"`
}

// The outcomes of a save that recorded a revision.
const (
	// ProjectConfigApplied: the stored settings hold the saved values and the
	// running service uses them.
	ProjectConfigApplied = "applied"
	// ProjectConfigSavedNotApplied: the stored settings hold the saved values
	// but the running service does not use them yet; they stay pending across
	// a reload, and a later save with the new pin or a restart applies them.
	ProjectConfigSavedNotApplied = "saved_not_applied"
)

// ProjectConfigSaveResponse reports a save that recorded a revision. It carries
// the snapshot of the saved revision, with the new pin. A save that recorded
// nothing is an error response instead.
type ProjectConfigSaveResponse struct {
	ProjectConfigResponse
	Outcome string `json:"outcome" enum:"applied,saved_not_applied"`
	Reason  string `json:"reason,omitempty" description:"Why the saved values are not applied."`
}
