package api

import "github.com/kpenfound/osmia/internal/config"

// ProjectConfigSetting is one setting of a project's config.toml. Value is
// what the file on disk gives it, or would give it: its default, or for
// capacity.per_workstream the top-level value. Default says the file leaves
// the setting out. Effective is the value in the loaded configuration, and
// Pending says the file's value is not yet applied. When the file does not
// load, only Name and Effective are present.
type ProjectConfigSetting struct {
	Name      string `json:"name" description:"Setting name: name, upstream, fork, clone, base_branch, landing, classifier, upstream_rebase, pull_request_poll, checks_timeout, capacity.per_workstream, capacity.git_operations, hearsay_scope, hearsay_declare_repo or hearsay_entities."`
	Value     any    `json:"value,omitempty" description:"The file's value, or the value it would take: a string, an integer, a boolean for hearsay_declare_repo, or for hearsay_entities an object of local entity ID to Hearsay entity id."`
	Default   *bool  `json:"default,omitempty" description:"True when the file leaves the setting out and Value is its default or inherited value."`
	Effective any    `json:"effective" description:"The value in the loaded configuration."`
	Pending   bool   `json:"pending" description:"True when Value differs from Effective: the file holds an edit that is not yet applied."`
}

// ProjectConfigError is the error a project file fails to load with.
type ProjectConfigError struct {
	Field   string `json:"field,omitempty" description:"The setting at fault, empty when the whole file is."`
	Message string `json:"message" description:"The loader's error for the file."`
}

// ProjectConfigResponse is the editable snapshot of one project's
// config.toml: the settings, the markers and the pin all describe the same
// bytes on disk. When the file does not load, Error says why, Settings hold
// the values in effect and Pin is empty, so no save is possible.
type ProjectConfigResponse struct {
	Project  config.ProjectID       `json:"project"`
	Path     string                 `json:"path" description:"The configuration file the settings come from."`
	Pin      string                 `json:"pin,omitempty" description:"Identifies the exact file bytes; send it with a save."`
	Error    *ProjectConfigError    `json:"error,omitempty"`
	Settings []ProjectConfigSetting `json:"settings"`
}

// ProjectConfigValues holds the settings a save sets; an omitted setting is
// not changed. An empty string for name, fork, classifier or hearsay_scope
// clears it.
type ProjectConfigValues struct {
	Name                  *string           `json:"name,omitempty"`
	Upstream              *string           `json:"upstream,omitempty"`
	Fork                  *string           `json:"fork,omitempty"`
	Clone                 *string           `json:"clone,omitempty"`
	BaseBranch            *string           `json:"base_branch,omitempty"`
	Landing               *string           `json:"landing,omitempty"`
	Classifier            *string           `json:"classifier,omitempty"`
	UpstreamRebase        *string           `json:"upstream_rebase,omitempty"`
	PullRequestPoll       *string           `json:"pull_request_poll,omitempty"`
	ChecksTimeout         *string           `json:"checks_timeout,omitempty"`
	CapacityPerWorkstream *int              `json:"capacity.per_workstream,omitempty"`
	CapacityGitOperations *int              `json:"capacity.git_operations,omitempty"`
	HearsayScope          *string           `json:"hearsay_scope,omitempty" description:"A Hearsay entity id, such as work:<source>:<project>, that replaces the project's default read entity."`
	HearsayDeclareRepo    *bool             `json:"hearsay_declare_repo,omitempty" description:"True posts the project to Hearsay with its GitHub repo declared, which makes its records visible to that repository's readers. Default false."`
	HearsayEntities       map[string]string `json:"hearsay_entities,omitempty" description:"Entries to set, by local entity ID, each a Hearsay entity id."`
}

// ProjectConfigEdit is a save of one project's config.toml: the pin of the
// snapshot the owner edited and the changes. Clear names settings whose key
// is removed from the file so the default or inherited value applies again,
// and ClearHearsayEntities names hearsay_entities entries to remove.
type ProjectConfigEdit struct {
	Pin                  string              `json:"pin"`
	Set                  ProjectConfigValues `json:"set"`
	Clear                []string            `json:"clear,omitempty"`
	ClearHearsayEntities []string            `json:"clear_hearsay_entities,omitempty"`
}

// The outcomes of a save that replaced the file.
const (
	// ProjectConfigApplied: the file holds the saved values and the running
	// service uses them.
	ProjectConfigApplied = "applied"
	// ProjectConfigSavedNotApplied: the file holds the saved values but the
	// running service does not use them yet; a reload or a later save with
	// the new pin applies them.
	ProjectConfigSavedNotApplied = "saved_not_applied"
)

// ProjectConfigSaveResponse reports a save that replaced the file. It carries
// the snapshot of the saved bytes, with the new pin. A save that did not
// replace the file is an error response instead.
type ProjectConfigSaveResponse struct {
	ProjectConfigResponse
	Outcome string `json:"outcome" enum:"applied,saved_not_applied"`
	Reason  string `json:"reason,omitempty" description:"Why the saved values are not applied."`
}
