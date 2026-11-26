// Package api defines the local API wire types shared by the service and its clients.
package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/engine"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const Prefix = "/v1"

type Code string

const (
	Malformed       Code = "malformed_input"
	Validation      Code = "validation"
	Conflict        Code = "conflict"
	Unsupported     Code = "unsupported"
	RestartRequired Code = "restart_required"
	ReloadRequired  Code = "reload_required"
	Unavailable     Code = "unavailable"
	Internal        Code = "internal"
	NoProject       Code = "no_project"
	ProjectActive   Code = "project_active"
	NotFound        Code = "not_found"
	Forbidden       Code = "forbidden"
	// TranscriptionOff answers a transcription request while no [transcribe]
	// url is configured.
	TranscriptionOff Code = "transcription_off"
	// TranscriptionFailed answers a transcription request the configured
	// server could not complete: unreachable, timed out, a non-success status
	// or an unreadable reply.
	TranscriptionFailed Code = "transcription_failed"
	// TooLarge answers an upload over its size limit.
	TooLarge Code = "too_large"
)

type APIError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Field names the configuration setting a validation refusal is about,
	// when there is one.
	Field string `json:"field,omitempty"`
}

func (e *APIError) Error() string { return string(e.Code) + ": " + e.Message }

type ErrorResponse struct {
	Error APIError `json:"error"`
}

type Identity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// HealthResponse reports the API and the backend separately: API is true
// whenever this response is sent at all, since the handler answers it
// without touching the trace or the activity record's lock. Backend is
// "ready" with no blocking work, "not_ready" while startup or project
// recovery is in progress, and "degraded" when a backend operation has
// failed or made no progress for ActivityStallThreshold; Cause then names
// the operation. Ready is true exactly when Backend is "ready".
type HealthResponse struct {
	Ready      bool     `json:"ready"`
	API        bool     `json:"api"`
	Backend    string   `json:"backend"`
	Cause      string   `json:"cause,omitempty"`
	Service    string   `json:"service"`
	APIVersion int      `json:"api_version"`
	Build      Identity `json:"build"`
	// Tailnet is the tailnet listener's state; it is omitted unless
	// listen.tailnet is configured.
	Tailnet *TailnetStatus `json:"tailnet,omitempty"`
}

// Backend states HealthResponse.Backend reports.
const (
	BackendReady    = "ready"
	BackendNotReady = "not_ready"
	BackendDegraded = "degraded"
)

// Tailnet listener states.
const (
	TailnetUp         = "up"
	TailnetConnecting = "connecting"
	TailnetNeedsLogin = "needs_login"
	TailnetDown       = "down"

	TailnetHTTPSOff         = "off"
	TailnetHTTPSUp          = "up"
	TailnetHTTPSUnavailable = "unavailable"
)

// TailnetStatus is the state of the tailnet listener: one of the Tailnet
// constants. LoginURL is the address that approves the node, given with
// needs_login when Tailscale supplies one. Reason says why the listener is
// down. HTTPSState is off, up or unavailable; HTTPSReason explains non-up
// states, and HTTPSAddress is the full MagicDNS URL only while HTTPS is up.
type TailnetStatus struct {
	State        string `json:"state"`
	LoginURL     string `json:"login_url,omitempty"`
	Reason       string `json:"reason,omitempty"`
	HTTPSState   string `json:"https_state"`
	HTTPSReason  string `json:"https_reason,omitempty"`
	HTTPSAddress string `json:"https_address,omitempty"`
}

type Diagnostic struct {
	Field   string `json:"field"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// ConfigResponse contains validated fields only, never raw configuration text.
// Project is the only active project: null until a project is registered and
// while several are active. Projects lists every active project, ordered by
// the creation time of each project's newest workstream, newest first. Every
// kind of workstream counts, archived ones included while their creation time
// can be read. Projects with equal newest times, and projects without a
// workstream, which come last, keep the project registry's order. Callers who
// need the registry's order read Effective.ActiveProjects.
// LastError is the last failed reload, until a reload succeeds.
type ConfigResponse struct {
	Root        string         `json:"root"`
	Digest      string         `json:"digest"`
	Effective   *config.Config `json:"effective"`
	Project     *ProjectView   `json:"project"`
	Projects    []ProjectView  `json:"projects,omitempty" description:"Every active project, ordered by the creation time of its newest workstream (feature workstream, workgroup member, design session or exploratory session; archived ones count while their creation time can be read), newest first. Projects with equal newest times, and projects without a workstream, which come last, keep the project registry's order. Read effective.active_projects for the registry's order."`
	Diagnostics []Diagnostic   `json:"diagnostics"`
	LastError   *ReloadError   `json:"last_error,omitempty"`
	// Drift compares the top-level configuration file on disk with the loaded
	// configuration.
	Drift ConfigDrift `json:"drift"`
	// Tailnet is the tailnet listener's state; it is omitted unless
	// listen.tailnet is configured.
	Tailnet *TailnetStatus `json:"tailnet,omitempty"`
}

// The states of a configuration file on disk against the loaded
// configuration.
const (
	ConfigUnchanged = "unchanged"
	ConfigChanged   = "changed"
	ConfigInvalid   = "invalid"
)

// ConfigDrift lists the top-level configuration file, the only file a reload
// reads. Project settings are not files and never appear. Differs is true when
// the file is not unchanged.
type ConfigDrift struct {
	Differs bool         `json:"differs"`
	Files   []ConfigFile `json:"files"`
}

// ConfigFile is one configuration file's State against the loaded
// configuration. Reason says why an invalid file fails, as a reload reports it.
type ConfigFile struct {
	Path   string `json:"path"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// ReloadResponse reports an applied reload: the digest of the configuration
// now loaded and each changed setting that keeps its loaded value until the
// service restarts.
type ReloadResponse struct {
	Digest          string   `json:"digest"`
	RestartRequired []string `json:"restart_required"`
}

// ReloadError is a reload that failed validation: the file and field at
// fault when known, a message naming them, and when it failed.
type ReloadError struct {
	Path    string    `json:"path,omitempty"`
	Field   string    `json:"field,omitempty"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// ProjectView describes one project and its repository settings.
type ProjectView struct {
	ID         config.ProjectID `json:"id"`
	Name       string           `json:"name"`
	Upstream   string           `json:"upstream"`
	Fork       string           `json:"fork,omitempty"`
	Clone      string           `json:"clone"`
	BaseBranch string           `json:"base_branch"`
	Trace      string           `json:"trace"`
	Repos      []RepoView       `json:"repos"`
	// CharterState is reported by status only.
	CharterState *CharterState `json:"charter_state,omitempty"`
}

// RepoView names one repo of a project: its own upstream, optional fork,
// local clone, landing style, trace and entity map.
type RepoView struct {
	ID         config.ProjectID `json:"id"`
	Name       string           `json:"name"`
	Upstream   string           `json:"upstream"`
	Fork       string           `json:"fork,omitempty"`
	Clone      string           `json:"clone"`
	BaseBranch string           `json:"base_branch"`
	Landing    string           `json:"landing"`
	Trace      string           `json:"trace"`
	KB         string           `json:"kb"`
	EntityMap  string           `json:"entity_map"`
}

// ProjectRebaseRequest asks for a drift rebase of every building or
// assembled workstream of the active project that is not paused.
type ProjectRebaseRequest struct {
	Project config.ProjectID `json:"project"`
}

// ProjectRebaseResponse reports a recorded drift rebase request: Covered
// holds each workstream it asks a drift rebase for, with the number of the
// drift rebase that answers it, and Skipped each other workstream with why.
type ProjectRebaseResponse struct {
	Project config.ProjectID `json:"project"`
	Covered []DriftCoverage  `json:"covered"`
	Skipped []DriftSkip      `json:"skipped"`
}

// WorkstreamRebaseResponse reports a recorded request for a drift rebase of
// one workstream, with the number of the drift rebase that answers it.
type WorkstreamRebaseResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Drift      int                 `json:"drift"`
}

// DriftCoverage is one workstream a drift rebase request covers.
type DriftCoverage struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Drift      int                 `json:"drift"`
}

// DriftSkip is one workstream a drift rebase request skips, and why.
type DriftSkip struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Reason     string              `json:"reason"`
}

// CharterState summarizes the charter as last recorded: its citable rules,
// its revision and its numbering diagnostics.
type CharterState struct {
	Rules       int                  `json:"rules"`
	Revision    int                  `json:"revision"`
	Diagnostics []charter.Diagnostic `json:"diagnostics"`
}

// HandInRequest hands work to the ordered Projects, whose first project hosts
// the shared chief and conversation. Project is a one-project shorthand.
// Exactly one of Path (an absolute file path), URL (a GitHub issue URL) and
// Stdin (the input itself) is
// set. Key identifies the request: a retry with the same key returns the same
// workstream. Base belongs to a selected project and applies only to its member.
// Kind is the workstream kind, which defaults to "feature" and is recorded once,
// in the commit that creates the workstream; a kind the service does not
// register is refused.
// SkipDebate skips every member's debate; ratification still belongs to the owner.
type HandInRequest struct {
	Base       config.WorkstreamID `json:"base,omitempty"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Projects   []config.ProjectID  `json:"projects,omitempty"`
	Kind       string              `json:"kind,omitempty"`
	Key        string              `json:"key"`
	Path       string              `json:"path,omitempty"`
	URL        string              `json:"url,omitempty"`
	Stdin      *string             `json:"stdin,omitempty"`
	SkipDebate bool                `json:"skip_debate,omitempty"`
}

// ProjectSelectionRequest supplies the owner's ordered projects for a held design start.
type ProjectSelectionRequest struct {
	Projects []config.ProjectID `json:"projects"`
}

// HandInResponse names the workstream a hand-in created. Source is where its
// input came from, as recorded. SkipDebate reports that the hand-in skipped
// the workstream's debate.
type HandInResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Source     string              `json:"source"`
	SkipDebate bool                `json:"skip_debate,omitempty"`
}

// DesignOpenRequest opens a design session in a project. The project may be
// left out while exactly one project is active. NewProject opens the session
// to design a project that does not exist yet: it is still hosted in an
// existing project, the named one or else the first active project.
type DesignOpenRequest struct {
	Project    config.ProjectID `json:"project"`
	NewProject bool             `json:"new_project,omitempty"`
}

// Repository visibilities of a new project.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
)

// NewProject is the project a design brief asks Start to create: its display
// name, its repository name under the owner's GitHub account, the repository's
// visibility (private unless the brief says public), its description and its
// first charter.
type NewProject struct {
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	Visibility  string `json:"visibility"`
	Description string `json:"description,omitempty"`
	Charter     string `json:"charter"`
}

// DesignView is a design session and its latest brief, revision and ordered
// projects. BriefRevision is zero until a brief is recorded. The session stays
// outside feature work until the owner starts a workgroup from the brief.
type DesignView struct {
	Workstream    config.WorkstreamID `json:"workstream"`
	Project       config.ProjectID    `json:"project"`
	CreatedAt     time.Time           `json:"created_at"`
	BriefRevision int                 `json:"brief_revision"`
	Brief         string              `json:"brief"`
	Projects      []config.ProjectID  `json:"projects"`
	// Kind is the workstream kind the latest brief sets, "feature" when the
	// brief never set one.
	Kind string `json:"kind"`
	// Title is the latest title the design session's agent set; it is omitted
	// for a session that has none.
	Title string `json:"title,omitempty"`
	// NewProject is the project the latest brief asks Start to create, if any.
	NewProject *NewProject `json:"new_project,omitempty"`
}

// DesignResponse answers opening or reading one design session.
type DesignResponse struct {
	Design DesignView `json:"design"`
}

// DesignsResponse lists design sessions, newest first.
type DesignsResponse struct {
	Designs []DesignView `json:"designs"`
}

// DesignMessageRequest is an owner message to a design session's agent.
type DesignMessageRequest struct {
	Text string `json:"text"`
}

// DesignStartRequest optionally pins the brief the owner reviewed.
type DesignStartRequest struct {
	BriefRevision *int `json:"brief_revision,omitempty"`
}

// DesignStartResponse names the workstream a design session's brief started
// a workgroup on. Handed is the path of the recorded brief; Source names the
// design session it came from. The workgroup's chief of staff opens a new
// thread from that brief and the design transcript attached to it; the design
// conversation's own thread is not continued.
type DesignStartResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Handed     string              `json:"handed"`
	Source     string              `json:"source"`
}

// AbandonRequest abandons a workstream for the owner's reason.
type AbandonRequest struct {
	Reason string `json:"reason"`
}

// AbandonResponse reports an abandoned workstream and the recorded reason.
type AbandonResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Reason     string              `json:"reason"`
}

// MergeResponse reports a workstream recorded delivered by the owner's own
// merge of its feature branch into upstream. Refused names the owner
// approval whose publication, waiting for a GitHub token, the merge
// refused, and is 0 when none waited.
type MergeResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Merge      DeliveryMerge       `json:"merge"`
	Refused    int                 `json:"refused_publication,omitempty"`
}

// BaseUpstreamRequest is the owner's decision to continue a workstream
// parked on its abandoned base from upstream. Base names that abandoned
// workstream, as the inbox entry the owner read names it.
type BaseUpstreamRequest struct {
	Base config.WorkstreamID `json:"base"`
}

// BaseUpstreamResponse reports a workstream moved onto upstream: the
// abandoned base, the commit of it the workstream held, found already in
// upstream, and the upstream commit the workstream continues from. Its
// feature branch moves there with its next drift rebase.
type BaseUpstreamResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Base       config.WorkstreamID `json:"base"`
	Integrated string              `json:"integrated,omitempty"`
	Upstream   seal.Base           `json:"upstream"`
}

// BaseDecisionUpstream is the decision a parked base's inbox entry offers
// when the workstream's own base was abandoned: continue from upstream.
const BaseDecisionUpstream = "upstream"

// The actors that archive a workstream: the owner by request, or the chief of
// staff once the workstream's own pull request merged.
const (
	ArchivedByOwner = "owner"
	ArchivedByChief = "chief_of_staff"
)

// ArchiveResponse reports whether a workstream is archived after the
// owner's request. ArchivedBy names who archived it, owner or chief_of_staff,
// and is empty while the workstream is not archived.
type ArchiveResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Archived   bool                `json:"archived"`
	ArchivedBy string              `json:"archived_by,omitempty"`
}

// ShedObjectRequest adds the owner's own objection to the current round.
type ShedObjectRequest struct {
	Argument string `json:"argument"`
}

// ShedRuleRequest rules on one objection that stands: disposition is sustain
// or dismiss, and note is the owner's reason, which may be empty.
type ShedRuleRequest struct {
	Objection   string `json:"objection"`
	Disposition string `json:"disposition"`
	Note        string `json:"note,omitempty"`
}

// ShedOverruleRequest overrules one objection that stands, with the owner's
// reason, which may be empty.
type ShedOverruleRequest struct {
	Objection string `json:"objection"`
	Reason    string `json:"reason,omitempty"`
}

// ShedMoreRequest asks for further rounds of debate after it concluded.
type ShedMoreRequest struct {
	Rounds int `json:"rounds"`
}

// ShedRedraftRequest asks the architect for a redraft of the spec and the
// plan, with the owner's note saying what to change.
type ShedRedraftRequest struct {
	Note string `json:"note"`
}

// PacketResponse is the ratification packet of a workstream: the revisions the
// owner decides on, why debate ended, the dissent record with what blocks
// ratification first, and the chief of staff's recommendation. ProjectSpec
// is present only when the workstream belongs to a multi-member workgroup
// whose project spec is already drafted: ratifying this packet then seals
// that project spec and every member, not only this workstream.
type PacketResponse struct {
	ProjectSpecRevision int                 `json:"project_spec_revision,omitempty"`
	Project             config.ProjectID    `json:"project"`
	Workstream          config.WorkstreamID `json:"workstream"`
	Packet              shed.Packet         `json:"packet"`
	Revision            int                 `json:"revision"`
	At                  time.Time           `json:"at"`
	ProjectSpec         *trace.ProjectSpec  `json:"project_spec,omitempty"`
}

// RatifyRequest ratifies the exact revisions of the spec and the plan the
// owner read in the packet.
type RatifyRequest struct {
	ProjectSpecRevision int `json:"project_spec_revision,omitempty"`
	Spec                int `json:"spec"`
	Plan                int `json:"plan"`
}

// RatifyResponse reports a recorded ratification: the revisions it approves,
// the round it was given in, and the state of its sealing: requested by this
// call, or pending or running from an earlier one. Members lists every
// workstream this one ratification sealed; it names only the named
// workstream for a one-workstream workgroup.
type RatifyResponse struct {
	Project    config.ProjectID      `json:"project"`
	Workstream config.WorkstreamID   `json:"workstream"`
	Round      int                   `json:"round"`
	Spec       int                   `json:"spec"`
	Plan       int                   `json:"plan"`
	Sealing    string                `json:"sealing"`
	Detail     string                `json:"detail"`
	Members    []config.WorkstreamID `json:"members,omitempty"`
}

// ShedResponse reports one owner action in the shed: what was recorded, the
// round it was recorded under and, for an objection or a ruling, which
// objection it concerns. Rounds is what a request for further debate asked
// for.
type ShedResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Action     string              `json:"action"`
	Round      int                 `json:"round"`
	Objection  string              `json:"objection,omitempty"`
	Rounds     int                 `json:"rounds,omitempty"`
	Detail     string              `json:"detail"`
}

// ProjectAddRequest registers a project. Clone is an absolute path to an
// existing local Git repository; base_branch defaults to main. Omitting Fork
// delivers feature branches and pull requests within Upstream.
type ProjectAddRequest struct {
	Name       string `json:"name"`
	Upstream   string `json:"upstream"`
	Fork       string `json:"fork,omitempty"`
	Clone      string `json:"clone"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// ProjectCreateRequest creates a project: its GitHub repository under the
// configured account, a clone in the configured clone directory, and the
// project's registration. ID is the creation identity, obtained from the
// identity route; leaving it empty reserves one. Repeating a request with the
// same ID finishes the same creation, and a request that carries only the ID
// finishes a creation already planned under it. Visibility is private unless
// it is "public". Charter is recorded as the project's first charter
// revision.
type ProjectCreateRequest struct {
	ID          config.ProjectID `json:"id,omitempty"`
	Name        string           `json:"name"`
	Repo        string           `json:"repo"`
	Description string           `json:"description,omitempty"`
	Visibility  string           `json:"visibility,omitempty"`
	Charter     string           `json:"charter"`
}

// ProjectCreationIdentity is a reserved creation identity: the ID the
// project will have. Nothing is recorded or created under it until a create
// request carries it.
type ProjectCreationIdentity struct {
	ID config.ProjectID `json:"id"`
}

// Creation states.
const (
	CreationNotStarted = "not_started"
	CreationPending    = "pending"
	CreationComplete   = "complete"
)

// Why a pending creation is stuck. Retrying never adopts a repository: it
// reuses only the repository this creation recorded.
const (
	// StuckOwnership: a repository of that name exists and the saved plan
	// holds no evidence that this creation made it, or GitHub refused the
	// create because the name is taken, or the recorded repository is no
	// longer found under the name.
	StuckOwnership = "ownership_not_established"
	// StuckMismatch: GitHub's create answer named a repository other than
	// the planned one.
	StuckMismatch = "repository_mismatch"
	// StuckRefused: GitHub refused the create; no repository was made.
	StuckRefused = "create_refused"
	// StuckClone: the clone failed after the repository exists.
	StuckClone = "clone_failed"
	// StuckBranch: the repository could not be given a main branch.
	StuckBranch = "main_branch_failed"
	// StuckUnknown: the create call's outcome is unknown, as after a
	// transport error, a timeout or a server error. A retry sends the create
	// again only when no repository of that name exists.
	StuckUnknown = "create_outcome_unknown"
)

// Steps a pending creation has reached.
const (
	CreationPlanned    = "planned"
	CreationRepository = "repository_created"
	CreationCloned     = "cloned"
	CreationMain       = "main_ready"
)

// ProjectCreationStuck says why a pending creation cannot go on without the
// owner.
type ProjectCreationStuck struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// ProjectCreationState is where the creation under one identity stands.
// Pending reports the last step reached and, when the creation cannot go on
// without the owner, why. Complete reports the registered project.
type ProjectCreationState struct {
	ID         config.ProjectID      `json:"id"`
	State      string                `json:"state"`
	Step       string                `json:"step,omitempty"`
	Repository string                `json:"repository,omitempty"`
	Stuck      *ProjectCreationStuck `json:"stuck,omitempty"`
	Project    *ProjectView          `json:"project,omitempty"`
}

type ProjectRemoveRequest struct {
	Project config.ProjectID `json:"project"`
}

type ProjectResponse struct {
	Project  ProjectView `json:"project"`
	NextStep string      `json:"next_step"`
}

type RuntimeResponse struct {
	Effective runtime.State               `json:"effective"`
	Profiles  map[string]EffectiveProfile `json:"profiles"`
	// Projects lists each active project's context mode, in the order of
	// ConfigResponse.Projects.
	Projects []ProjectRuntime `json:"projects" description:"Each active project's context mode, in the same order as the projects of the configuration read: by the creation time of the project's newest workstream, newest first, with ties and projects without a workstream in registry order."`
	// Jev reports Jev's mode. Every mode but ready means its judgments fall
	// back.
	Jev         jev.Status   `json:"jev"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// EffectiveProfile identifies the profile selected for a role's next turn.
type EffectiveProfile struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
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

type ClearProviderLimitRequest struct {
	Backend string `json:"backend"`
}

type MutationResponse struct {
	Applied bool `json:"applied"`
}

type StopResponse struct {
	Stopping bool `json:"stopping"`
}

// StatusResponse lists every workstream of the active project and every role's
// effective profile. Workstreams is empty without an active project or trace,
// or when the trace cannot be read, which a diagnostic reports. DailyBudget is
// null without budget.per_day or when today's spend cannot be read, which a
// diagnostic reports.
type StatusResponse struct {
	Workstreams    []WorkstreamStatus          `json:"workstreams"`
	Profiles       map[string]EffectiveProfile `json:"profiles"`
	ProviderLimits []runtime.ProviderLimit     `json:"provider_limits,omitempty"`
	DailyBudget    *DailyBudgetStatus          `json:"daily_budget"`
	// Capacity is null when the turns of the active project cannot be read,
	// which a diagnostic reports.
	Capacity *CapacityStatus `json:"capacity"`
	// ProviderUsage is null when today's costs or turn attempts cannot be
	// read, which a diagnostic reports.
	ProviderUsage *ProviderUsageStatus `json:"provider_usage"`
	// FailureStreaks lists the roles and profiles whose latest turn attempts
	// failed with infrastructure failures; it is omitted when there are none.
	FailureStreaks []FailureStreak `json:"failure_streaks,omitempty"`
	Diagnostics    []Diagnostic    `json:"diagnostics"`
	// Tailnet is the tailnet listener's state; it is omitted unless
	// listen.tailnet is configured.
	Tailnet *TailnetStatus `json:"tailnet,omitempty"`
	// Workspaces is the workspace backend new workstreams get. With
	// workspaces = "jujutsu" and no supported jj, a diagnostic also
	// reports its problem.
	Workspaces WorkspacesStatus `json:"workspaces"`
}

// WorkspacesStatus is the backend new workstreams get. Setting is the
// configured workspaces value and Backend what a new workstream is created
// on, empty when none can be: workspaces = "jujutsu" without a supported jj,
// which Problem then describes. JJ is the jj version found, empty when none
// was found or the setting is git, which checks for none.
type WorkspacesStatus struct {
	Setting string `json:"setting"`
	Backend string `json:"backend"`
	JJ      string `json:"jj,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// FailureStreak counts the consecutive infrastructure failures of one role's
// turn attempts on one profile, across the active project's workstreams. Any
// attempt on that profile that ends without one resets the count; an attempt
// the service stopped or interrupted leaves it as it is. LastFailure and
// LastAt are the latest failure and the time its attempt started.
type FailureStreak struct {
	Role        string    `json:"role"`
	Profile     string    `json:"profile"`
	Consecutive int       `json:"consecutive"`
	LastFailure string    `json:"last_failure"`
	LastAt      time.Time `json:"last_at"`
	// Clients names the clients the failed attempts ran on, in order of first
	// failure.
	Clients []string `json:"clients,omitempty"`
}

// DailyBudgetStatus is the known spend of the service host's current local
// calendar day, Day as YYYY-MM-DD, across every workstream, against the
// configured budget.per_day. Amounts are decimal USD strings. UnknownCosts
// counts that day's attempts whose cost is unknown; when there are any,
// LowerBound is true and actual spend may be higher.
type DailyBudgetStatus struct {
	Day          string `json:"day"`
	SpendUSD     string `json:"spend_usd"`
	LimitUSD     string `json:"limit_usd"`
	UnknownCosts int    `json:"unknown_costs"`
	LowerBound   bool   `json:"lower_bound"`
}

// CapacityStatus reports shared role and check slot accounting and the
// per-workstream worker-turn limit in force.
type CapacityStatus struct {
	PerWorkstream int            `json:"per_workstream" description:"Maximum concurrent worker turns per workstream; check slots are independent."`
	Roles         []RoleCapacity `json:"roles" description:"Factory-wide mason, reviewer, committee, architect, checks and chief slot pools, in that order."`
	Clients       []ClientStatus `json:"clients" description:"The local client and every connected client, in the order new turns rotate through them. Each client's slots are a second bound next to the role pools."`
}

// The reasons a client takes no new turn now, the values of
// ClientStatus.Idle.
const (
	// ClientIdleDraining: the client is draining.
	ClientIdleDraining = "draining"
	// ClientIdleNoSlots: the local client is set to no slots, or a client
	// declared none.
	ClientIdleNoSlots = "no-slots"
	// ClientIdleFull: every slot of the client is taken.
	ClientIdleFull = "full"
	// ClientIdleNoReport: the client has not reported what it can run yet.
	ClientIdleNoReport = "no-report"
	// ClientIdleNoProfile: the client can run no profile.
	ClientIdleNoProfile = "no-profile"
)

// The health of a client's lease, the values of ClientLease.Health.
const (
	// LeaseHealthy: the client was heard from recently.
	LeaseHealthy = "healthy"
	// LeaseLate: heartbeats are missing but the grace window has not passed.
	LeaseLate = "late"
)

// ClientStatus is one client as the owner sees it: the local client, which is
// the server's own machine, or a connected client identified by its tailnet
// node. Turns lists the turns running on it, Profiles what it can and cannot
// run with the fix for each missing item, Skipped the fallback profiles it
// skips, and Failures the latest turns that failed on it. A client that takes
// no new turn says why in Idle and IdleReason.
type ClientStatus struct {
	ID    string `json:"id" description:"The client's identity: its tailnet node's stable ID, or local for the server's own machine."`
	Name  string `json:"name" description:"The tailnet node's name, or local for the server's own machine."`
	Local bool   `json:"local,omitempty" description:"True for the built-in local client."`
	Slots *int   `json:"slots,omitempty" description:"The client's slot count. Absent when unbounded is true."`
	// Unbounded is true for a local client with no slot setting, which adds no cap beyond the role limits.
	Unbounded bool `json:"unbounded,omitempty" description:"True for the local client when capacity.local_slots is unset: it adds no cap beyond the role limits."`
	Used      int  `json:"used" description:"Slots the client's turns and offers take."`
	// Lease is absent for the local client, which holds no lease.
	Lease    *ClientLease `json:"lease,omitempty" description:"The client's lease. Absent for the local client."`
	Draining bool         `json:"draining" description:"True while the client is draining: it takes no new turns."`
	Idle     string       `json:"idle,omitempty" enum:"draining,no-slots,full,no-report,no-profile" description:"Why the client takes no new turn now. Absent when it takes one."`
	// IdleReason says Idle in words, with the fix when there is one.
	IdleReason string `json:"idle_reason,omitempty" description:"Idle in plain words, with the fix when there is one."`
	// Reported is false until a connected client sends its capability report.
	Reported bool `json:"reported" description:"True once the client has reported what it can run; the local client runs every profile."`
	// ReportCurrent is false when the report checked an older profile handout than the server holds now.
	ReportCurrent bool                  `json:"report_current" description:"False when the client's report checked an older profile handout than the server holds now."`
	Profiles      []ClientProfileStatus `json:"profiles" description:"Each configured profile the client can or cannot run."`
	Skipped       []ClientSkip          `json:"skipped" description:"Fallback profiles the client skips because it cannot run them."`
	Turns         []ClientTurn          `json:"turns" description:"The turns running on the client."`
	Failures      []ClientFailure       `json:"failures" description:"The latest turns that failed on the client, newest first."`
}

// ClientLease is the health of a connected client's lease.
type ClientLease struct {
	Health      string    `json:"health" enum:"healthy,late" description:"healthy when heartbeats arrive; late when they are missing but the grace window has not passed."`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen" description:"When the server last heard from the client."`
	EndsAt      time.Time `json:"ends_at" description:"When the lease ends unless the client is heard from."`
}

// ClientProfileStatus is whether a client can run one profile and, when it
// cannot, what it lacks and the fix.
type ClientProfileStatus struct {
	Profile  string           `json:"profile"`
	Runnable bool             `json:"runnable"`
	Problems []ProfileProblem `json:"problems,omitempty"`
}

// ClientSkip is a fallback profile a client skips: Profile follows Main in
// Role's chain and the client cannot run it.
type ClientSkip struct {
	Role    string `json:"role"`
	Main    string `json:"main" description:"The role's main profile, which the client can run."`
	Profile string `json:"profile" description:"The fallback profile the client skips."`
}

// ClientTurn is a turn running on a client.
type ClientTurn struct {
	Turn       string              `json:"turn"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Unit       string              `json:"unit,omitempty"`
	Role       string              `json:"role,omitempty"`
	Agent      string              `json:"agent,omitempty"`
	Profile    string              `json:"profile,omitempty"`
	StartedAt  *time.Time          `json:"started_at,omitempty"`
}

// ClientFailure is a turn that failed on a client. Class is infrastructure
// for a failure of the machine or its setup, such as a missing provider
// login, and behavioural for one of the agent's work.
type ClientFailure struct {
	Turn       string              `json:"turn"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit,omitempty"`
	Role       string              `json:"role"`
	Profile    string              `json:"profile"`
	Failure    string              `json:"failure"`
	Class      string              `json:"class,omitempty" enum:"infrastructure,behavioural"`
	At         time.Time           `json:"at"`
}

// RoleCapacity is one pool's slots: Used includes turns, workflow reservations
// or check runs. A pool with a configured factory-wide limit reports it in
// Limit. A pool without one (architect) sets Unbounded, omits
// Limit, and says in Scope and Note how it is bounded instead.
// Waiting lists work waiting for a slot and never paused work.
type RoleCapacity struct {
	Role      string     `json:"role" description:"Slot pool: mason, reviewer, committee, architect, checks or chief. The checks pool uses capacity.checks (default 2) across all projects and workstreams. The chief pool uses capacity.chief (default 3) across all projects and workstreams and holds background chief-of-staff turns only; turns the owner starts take no chief slot."`
	Used      int        `json:"used" description:"Number of slots currently in use."`
	Limit     *int       `json:"limit,omitempty" description:"Configured factory-wide slot limit from config.toml. Present only for bounded pools (mason, reviewer, committee, checks, chief). Absent when unbounded is true: that pool has no factory-wide limit, which is not a limit of 0."`
	Unbounded bool       `json:"unbounded,omitempty" description:"True when the pool has no factory-wide limit (architect). Such an entry has no limit field, scope is per_workstream and note explains it; there is no configuration setting to raise."`
	Scope     string     `json:"scope,omitempty" description:"How an unbounded pool is bounded instead: per_workstream means at most one turn runs per workstream. Absent for bounded pools."`
	Note      string     `json:"note,omitempty" description:"Plain-words explanation of an unbounded pool: no factory-wide limit, at most one turn per workstream, no configuration setting. Absent for bounded pools."`
	Waiting   []SlotWait `json:"waiting" description:"Work waiting for a slot. Check runs identify their project, workstream and unit, final review or session. Background chief turns identify their project, workstream, agent and turn, oldest first."`
}

// UnboundedScope is RoleCapacity.Scope of a pool that runs at most one turn
// per workstream and has no factory-wide limit.
const UnboundedScope = "per_workstream"

// UnboundedNote is RoleCapacity.Note of an unbounded pool.
const UnboundedNote = "No factory-wide limit: this pool runs at most one turn per workstream, and no configuration setting controls it. Nothing to raise; it is not zero capacity."

// SlotWait identifies a queued turn, a deferred ready unit, or a check run.
// For check runs, exactly one of Unit, Review, Session or Preview identifies the kind.
// Reason is capacity when every slot of the pool is taken, priority when
// higher-priority workstreams start a unit first, and workstream-cap when the
// workstream holds capacity.per_workstream agent turns. A queued turn whose
// role has room waits with reason client when the client it can run on has no
// free slot, which for a turn that resumes a session is the client holding it,
// and with reason no-client when no connected client can run its profile.
type SlotWait struct {
	Client     string              `json:"client,omitempty" description:"The client a resumed turn waits for a slot of: the one that holds its saved session."`
	Project    config.ProjectID    `json:"project,omitempty" description:"Project of a waiting check run."`
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit,omitempty"`
	Review     int                 `json:"review,omitempty" description:"Final review number for a waiting final-review check run."`
	Session    bool                `json:"session,omitempty" description:"True for a waiting session check run; workstream identifies the session."`
	Preview    bool                `json:"preview,omitempty" description:"True for a waiting service-run chief preview."`
	Agent      string              `json:"agent,omitempty"`
	Turn       string              `json:"turn,omitempty"`
	Reason     string              `json:"reason"`
}

// ProviderUsageStatus is the known spend of the service host's current local
// calendar day, Day as YYYY-MM-DD, by provider, next to each provider's
// usage limit. Unattributed holds the day's costs no recorded turn attempt
// names a provider for, and is omitted when there are none.
type ProviderUsageStatus struct {
	Day          string          `json:"day"`
	Providers    []ProviderUsage `json:"providers"`
	Unattributed *ProviderSpend  `json:"unattributed,omitempty"`
}

// ProviderSpend is a known spend as a decimal USD string. UnknownCosts counts
// the attempts whose cost is unknown; when there are any, LowerBound is true
// and actual spend may be higher.
type ProviderSpend struct {
	SpendUSD     string `json:"spend_usd"`
	UnknownCosts int    `json:"unknown_costs"`
	LowerBound   bool   `json:"lower_bound"`
}

// ProviderUsage is one provider's day. Limit is its usage limit in force, or
// null. Fallbacks lists the roles that run a fallback profile because the
// provider of their configured profile is limited, and PausedRoles those
// that are paused because no fallback is available.
type ProviderUsage struct {
	Provider string `json:"provider"`
	ProviderSpend
	Limit       *runtime.ProviderLimit `json:"limit"`
	Fallbacks   []RoleFallback         `json:"fallbacks"`
	PausedRoles []string               `json:"paused_roles"`
}

// RoleFallback is a role running Profile in place of its Configured profile.
type RoleFallback struct {
	Role       string `json:"role"`
	Configured string `json:"configured"`
	Profile    string `json:"profile"`
}

// WorkstreamStatus is the chief of staff's status for one workstream, next to
// the facts the service owns. State is null until a feature state is
// recorded; Units is empty until the units' states are recorded, and each
// unit carries its latest reported card when present; Advisories holds the
// active overlap advisories about other workstreams of the project; Gates is
// empty while no owner decision waits; Drift is null before the first drift
// rebase; Status is null until the chief of staff writes one. Agents holds
// service-owned facts about claimed and parked turns. Workspaces is the
// workspace backend the workstream was created on, git or jujutsu.
type WorkstreamStatus struct {
	// DisplayState is the project pane label derived from recorded service facts.
	DisplayState string              `json:"display_state"`
	Workgroup    *WorkgroupView      `json:"workgroup,omitempty"`
	Workstream   config.WorkstreamID `json:"workstream"`
	Project      config.ProjectID    `json:"project"`
	Workspaces   string              `json:"workspaces"`
	// Kind is "session" for a session workstream, "design" for a design
	// session that has not yet started a workgroup, omitted for an ordinary
	// one. Profile, Base and Branch are set only for a session: its fixed
	// profile, base commit and branch. Title is a session's optional title and
	// a design session's latest title.
	Kind    string  `json:"kind,omitempty"`
	Profile string  `json:"profile,omitempty"`
	Base    string  `json:"base,omitempty"`
	Branch  string  `json:"branch,omitempty"`
	Title   string  `json:"title,omitempty"`
	State   *string `json:"state"`
	// WorkKind is the registered kind of a workstream that is not a feature,
	// such as hill-climb. It is omitted for a feature workstream, a session
	// and a design session.
	WorkKind string `json:"work_kind,omitempty"`
	// Progress is the kind's reading of how far the workstream has got, for a
	// kind whose progress is not its units. It is omitted for a feature
	// workstream, which shows its units.
	Progress      *ProgressView     `json:"progress,omitempty"`
	Units         []UnitStatus      `json:"units"`
	Advisories    []OverlapAdvisory `json:"advisories"`
	OpenQuestions int               `json:"open_questions"`
	Gates         []trace.OwnerGate `json:"gates"`
	ContextMode   bundle.Mode       `json:"context_mode"`
	Drift         *DriftStatus      `json:"drift"`
	Status        *StatusView       `json:"status"`
	Agents        []AgentStatus     `json:"agents"`
	// Archived is set when the owner archived the workstream, which takes a
	// delivered or abandoned workstream out of the list of work.
	Archived bool `json:"archived,omitempty"`
	// ArchivedBy names who archived the workstream: owner, or chief_of_staff
	// when it archived the workstream because its pull request merged.
	ArchivedBy string `json:"archived_by,omitempty"`
	// TraceDeleted is set after permanent archive cleanup completes.
	TraceDeleted bool `json:"trace_deleted,omitempty"`
	// DeliveryLink is, for an archived workstream and where one exists, the
	// pull request URL or the recorded upstream commit of an owner-recorded
	// merge.
	DeliveryLink string `json:"delivery_link,omitempty"`
	// Wait names, in plain words, an edge or merge the workstream waits on,
	// for example "waiting on owner: repo-a#123 merge". It is "" while
	// nothing is waited on. A wait is status only: it never creates an
	// inbox entry or a notification.
	Wait string `json:"wait,omitempty"`
	// TelemetryTraceID and TelemetryRootSpanID are the workstream's
	// OpenTelemetry identity, derived from the workstream ID alone.
	TelemetryTraceID    string `json:"telemetry_trace_id" description:"OpenTelemetry trace ID of every span Osmia sends about the workstream, as 32 lowercase hexadecimal characters. It depends only on the workstream ID, so it is present whether or not telemetry export is configured and is the same after a restart. It is not the durable trace record store read through /v1/trace."`
	TelemetryRootSpanID string `json:"telemetry_root_span_id" description:"Span ID of the workstream's osmia.workstream root span, the parent of its state, turn and check spans, as 16 lowercase hexadecimal characters. With telemetry_trace_id it names a span context an API client can link to."`
}

// ProgressView is a kind's one-line progress summary and the sparkline the
// workstream list draws beside it. Panel names the web panel that shows the
// workstream's progress in place of its units. Spark holds the points of the
// sparkline, oriented so that a rising line is always an improvement, and is
// empty until there are two.
type ProgressView struct {
	Panel   string    `json:"panel"`
	Summary string    `json:"summary"`
	Spark   []float64 `json:"spark,omitempty"`
}

// WorkgroupView presents recorded membership and the workstream graph. It
// carries no workflow authority; the service's gates govern members.
type WorkgroupView struct {
	ID      config.WorkgroupID    `json:"id"`
	Members []config.WorkstreamID `json:"members"`
	Edges   []trace.Edge          `json:"edges"`
	// Dropped is the part of Edges whose dependent dropped the dependency with a
	// waiver in its own trace. The shared graph still names those edges, and a
	// dropped edge is never one whose upstream merged.
	Dropped []trace.Edge `json:"dropped,omitempty"`
	// Conversation names the original design address when that conversation
	// was adopted by this workgroup. Member feed endpoints use that authority.
	Conversation *trace.ConversationOrigin `json:"conversation,omitempty"`
}

// SessionOpenRequest opens a session: a registered project and a profile
// the service can run for mason turns there, with an optional title. No
// other session request type carries a profile: it is fixed once opened.
type SessionOpenRequest struct {
	Project config.ProjectID `json:"project"`
	Profile string           `json:"profile"`
	Title   string           `json:"title,omitempty"`
}

// SessionView is one session: its fixed profile, base commit and branch,
// its optional title, and its state (idle, working or ended).
type SessionView struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Project    config.ProjectID    `json:"project"`
	Profile    string              `json:"profile"`
	Base       string              `json:"base"`
	Branch     string              `json:"branch"`
	Title      string              `json:"title,omitempty"`
	State      string              `json:"state"`
	CreatedAt  time.Time           `json:"created_at"`
}

// SessionResponse answers opening or reading one session.
type SessionResponse struct {
	Session SessionView `json:"session"`
}

// SessionDestroyResponse confirms permanent removal without retaining history.
type SessionDestroyResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Destroyed  bool                `json:"destroyed"`
}

// SessionsResponse lists sessions, newest first.
type SessionsResponse struct {
	Sessions []SessionView `json:"sessions"`
}

// SessionMessageRequest is an owner message to a session's agent.
type SessionMessageRequest struct {
	Text string `json:"text"`
}

// SessionCommitSummary is one commit on a session branch: its hash and
// message.
type SessionCommitSummary struct {
	Commit  string `json:"commit"`
	Message string `json:"message"`
}

// SessionChangesResponse reads a session's changes: the worktree's
// uncommitted changes relative to its head, as a unified diff, and the
// commits on its branch relative to its base commit, oldest first. Changes
// is empty once the session has ended and its worktree is released; Commits
// stays readable from the branch's own history.
type SessionChangesResponse struct {
	Head    string                 `json:"head"`
	Changes string                 `json:"changes"`
	Commits []SessionCommitSummary `json:"commits"`
}

// SessionCommitRequest commits every current worktree change onto the
// session branch under the owner's message. RequestKey identifies the
// request, so a retry with the same key, including across a restart,
// produces exactly one commit instead of a second one.
type SessionCommitRequest struct {
	Message    string `json:"message"`
	RequestKey string `json:"request_key"`
}

// SessionCommitResponse is the result of a session commit: the commit the
// session branch now points at.
type SessionCommitResponse struct {
	Commit string `json:"commit"`
}

// SessionCheckRun is one check run of a session: the commit it checked,
// whether it was a smart or a full run and, for a smart run, the checks it
// selected, its status, exit code and output.
type SessionCheckRun struct {
	Run                int             `json:"run"`
	Commit             string          `json:"commit"`
	Kind               string          `json:"kind"`
	Checks             []string        `json:"checks,omitempty"`
	Selection          *CheckSelection `json:"selection,omitempty"`
	Command            []string        `json:"command,omitempty"`
	Status             string          `json:"status"`
	ExitCode           int             `json:"exit_code"`
	Output             string          `json:"output,omitempty"`
	OutputRevision     int             `json:"output_revision,omitempty"`
	OutputPath         string          `json:"output_path,omitempty"`
	Truncated          bool            `json:"truncated,omitempty"`
	Failed             []string        `json:"failed,omitempty"`
	Flaky              []string        `json:"flaky,omitempty"`
	TraceURL           string          `json:"trace_url,omitempty"`
	CIIdentityFallback string          `json:"ci_identity_fallback,omitempty"`
	Error              string          `json:"error,omitempty"`
	StartedAt          time.Time       `json:"started_at"`
	FinishedAt         time.Time       `json:"finished_at"`
}

// SessionPushResponse is the result of pushing a session branch.
type SessionPushResponse struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// AgentStatus is one claimed or parked turn from the durable thread snapshot.
type AgentStatus struct {
	// Client is the client the turn runs on: local, or a tailnet node's stable ID.
	Client     string    `json:"client,omitempty"`
	Role       string    `json:"role"`
	Unit       string    `json:"unit"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	Elapsed    int64     `json:"elapsed"`
	Profile    string    `json:"profile"`
	Attempt    int       `json:"attempt,omitempty"`
	Path       string    `json:"path,omitempty"`
	QuestionID string    `json:"question_id,omitempty"`
}

// DriftStatus is a workstream's latest drift rebase: its number, its
// outcome (requested while it has none), and when and why that was recorded.
// Moved holds, oldest first, the upstream moved events it raised for the
// chief of staff, one for each visible outcome; it is empty for a drift
// rebase with none.
type DriftStatus struct {
	Drift   int       `json:"drift"`
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
	Reason  string    `json:"reason"`
	Moved   []string  `json:"moved"`
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

// InboxResponse lists every owner decision of the active project that waits
// for the owner, oldest first.
type InboxResponse struct {
	Entries []InboxEntry `json:"entries"`
}

// The kinds of owner decision the inbox lists.
const (
	InboxEscalation   = "escalation"
	InboxRatification = "ratification"
	InboxContested    = "contested"
	InboxAmendment    = "amendment"
	InboxDelivery     = "delivery"
	InboxPublication  = "publication"
	InboxFinalChecks  = "checks"
	// InboxChecksChoice asks whether the final review of a push waits for
	// check results that did not appear or continues without them.
	InboxChecksChoice = "checks-choice"
	InboxDrift        = "drift"
	InboxNotices      = "notices"
	InboxLoop         = "loop"
	InboxBase         = "base"
	// InboxRecovery asks the owner to approve one recovery move: a stall that
	// neither Jev nor the chief of staff cleared, or a move that belongs to the
	// owner. The card pins the exact move.
	InboxRecovery = "recovery"
	// InboxDiscovery is a discovered piece of work whose disposition Jev
	// decided but the service could not carry out, so it is the owner's.
	InboxDiscovery = "discovery"
	// InboxProjectCreation reports a project creation that cannot finish
	// without the owner. Its answer retries the creation; it never adopts or
	// deletes a repository.
	InboxProjectCreation = "project-creation"
)

// InboxEntry is one open owner decision. Kind says which, and Answer how it
// is answered. Number and Batch identify an escalation, the questions the
// chief of staff sent the owner as one ask, and Asked holds its questions as
// their askers put them. Unit names a contested unit, or the unit an
// amendment was filed from; Amendment names an amendment. Revision is the
// revision of the record the decision is taken on. Question is the chief of
// staff's rephrasing of an escalation, and the service's statement of the
// decision for the other kinds. A publication entry reports a publication
// whose last attempts failed: it takes no decision, its Revision is the owner
// approval being published and its Options are empty. A checks entry reports
// a final review whose checks did not complete maxIncompleteChecks times in a
// row: it takes no decision, its Revision is the final review and its Options
// are empty. A checks-choice entry asks whether the final review of a push
// keeps waiting for check results that did not appear or continues without
// them: its Options are keep_waiting and continue, its Revision is the offer
// the answer belongs to, and its Answer body pins the push (number, pull
// request, pushed commit and the operation that recorded it). Project names
// the project whose workstream
// the decision belongs to; escalation numbers are unique only within it.
type InboxEntry struct {
	// Workgroup identifies a shared decision kept in the workgroup trace.
	Workgroup      config.WorkgroupID  `json:"workgroup,omitempty"`
	Kind           string              `json:"kind"`
	Project        config.ProjectID    `json:"project"`
	Workstream     config.WorkstreamID `json:"workstream"`
	Number         int                 `json:"number"`
	Batch          string              `json:"batch"`
	Unit           string              `json:"unit"`
	Amendment      string              `json:"amendment"`
	Revision       int                 `json:"revision"`
	Question       string              `json:"question"`
	Blocked        string              `json:"blocked"`
	Options        []string            `json:"options"`
	Recommendation string              `json:"recommendation"`
	QuickReply     string              `json:"quick_reply"`
	OpenedAt       time.Time           `json:"opened_at"`
	Asked          []InboxQuestion     `json:"asked"`
	Answer         InboxAnswer         `json:"answer"`
	// Land is the answer that approves a delivery entry and lands the
	// workstream on its base branch, with the same Body as Answer. It is
	// present only on a delivery entry of a project that lands, whose Options
	// then also hold "land".
	Land *InboxAnswer `json:"land,omitempty" description:"The answer that approves a delivery entry and lands the workstream. Present only on a delivery entry of a project whose delivery is jev, beside the Mark ready answer."`
	// Decline is the answer that declines the pull request of a Hill Climb
	// workstream that kept no attempt, so that it ends as concluded, no
	// improvement. It is present only on that delivery entry, whose Options then
	// also hold "decline".
	Decline *InboxAnswer `json:"decline,omitempty" description:"The answer that declines the evaluator-only pull request of a Hill Climb workstream that kept no attempt, which ends the workstream as concluded, no improvement. Present only on that delivery entry, beside the Mark ready answer."`
	// Jev is what Jev decided about the entry when the decision came to the
	// owner after all: an answer below the threshold, a deferral, or a pick
	// the service refused. Recommendation then says it in words.
	Jev *InboxJev `json:"jev,omitempty" description:"What Jev decided about the entry when the decision came to the owner after all: an answer below the threshold, a deferral, or a pick the service refused to apply. On a delivery entry it is Jev's delivery-route decision."`
	// Recovery is the exact move a recovery card pins; the answer body carries
	// the same pin.
	Recovery *RecoveryMove `json:"recovery,omitempty"`
}

// InboxJev is Jev's recorded result for an inbox entry. Pick is the option it
// chose, empty when it deferred, and Confidence its confidence against
// Threshold. Binding says the pick reached the threshold, so only a refusal
// brought the entry to the owner, and Refused is why the service refused it.
// Judgment names the judgment record.
type InboxJev struct {
	Pick       string  `json:"pick,omitempty"`
	Confidence float64 `json:"confidence"`
	Threshold  float64 `json:"threshold"`
	Deferred   bool    `json:"deferred,omitempty"`
	Binding    bool    `json:"binding,omitempty"`
	Refused    string  `json:"refused,omitempty"`
	Judgment   string  `json:"judgment"`
}

// InboxAnswer is the endpoint that answers an inbox entry, or that
// shows the delivery of a publication entry, which takes no answer. Body
// holds the request fields that pin what the owner read; the client adds the
// decision fields of that endpoint's request.
type InboxAnswer struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Body   map[string]any `json:"body"`
}

// InboxQuestion is one question of an escalation as its asker put it.
type InboxQuestion struct {
	ID       string `json:"id"`
	AskedBy  string `json:"asked_by"`
	Unit     string `json:"unit,omitempty"`
	Question string `json:"question"`
}

// AnswerRequest is the owner's ruling on an inbox entry.
type AnswerRequest struct {
	Text string `json:"text"`
	// Project names the project whose inbox numbers the entry. It may be
	// left out while exactly one project is active.
	Project config.ProjectID `json:"project,omitempty"`
}

// AnswerResponse reports a recorded ruling and the questions it covers.
type AnswerResponse struct {
	Number     int                 `json:"number"`
	Workstream config.WorkstreamID `json:"workstream"`
	Batch      string              `json:"batch"`
	Questions  []string            `json:"questions"`
	Ruling     string              `json:"ruling"`
	At         time.Time           `json:"at"`
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
	// TurnStopped is a session turn the owner stopped directly: it is
	// recorded as stopped, not as a failure, and the worktree changes it
	// already made are kept.
	TurnStopped TurnState = "stopped"
)

// ConversationResponse lists a workstream's conversation with its chief of
// staff, oldest first.
type ConversationResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Entries    []ConversationEntry `json:"entries"`
}

// ConversationEntry is an owner message (kind "message"), the chief of
// staff's final response to it (kind "response"), or something the chief of
// staff did on the owner's behalf (kind "action"), such as resolving a
// contested unit. A message and its response carry the state of the turn that
// answers the message, and At is when the message was accepted, the response
// captured or the action recorded.
type ConversationEntry struct {
	QueuedReason string    `json:"queued_reason,omitempty"`
	Turn         string    `json:"turn"`
	Kind         string    `json:"kind"`
	Text         string    `json:"text"`
	At           time.Time `json:"at"`
	State        TurnState `json:"state"`
}

// WorkgroupFeedResponse lists shared chief entries and current member summaries.
// Both single-member and multi-member workgroups can use this endpoint.
type WorkgroupFeedResponse struct {
	Workgroup config.WorkgroupID       `json:"workgroup"`
	Entries   []FeedEntry              `json:"entries"`
	Members   []WorkgroupMemberSummary `json:"members"`
}

// WorkgroupMemberSummary presents a member from live facts or its pinned archive.
// Archived unit counts are empty because archived history is not consulted.
type WorkgroupMemberSummary struct {
	Workstream   config.WorkstreamID `json:"workstream"`
	Repo         config.ProjectID    `json:"repo"`
	State        *string             `json:"state"`
	DisplayState string              `json:"display_state"`
	Units        map[string]int      `json:"units"`
	Wait         string              `json:"wait"`
	PRLink       string              `json:"pr_link"`
	Running      bool                `json:"running"`
	Archived     bool                `json:"archived"`
	// ArchivedBy names who archived the member, owner or chief_of_staff.
	ArchivedBy string `json:"archived_by,omitempty"`
}

// FeedResponse lists a workstream's feed, oldest first.
type FeedResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Entries    []FeedEntry         `json:"entries"`
}

// FeedImage is attachment metadata. Removed references have a placeholder and no URL.
type FeedImage struct {
	ID          string `json:"id"`
	URL         string `json:"url,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// FeedEntry is one event of a workstream's feed. Kind "message", "response"
// and "action" are the conversation with the chief of staff, as
// ConversationEntry describes them, with Turn, Text and State. Kind "status"
// is a status the chief of staff wrote, with Status. Kind "session" is an
// agent turn that started, with Session. Kind "transition" is any recorded
// workflow state change, with Transition. Kind "unit_checks" summarizes a
// unit check run, "final_checks" a final check attempt, and "final_review"
// a final review outcome. Kind "image" carries attachment metadata in Image,
// with a URL or an archived-member placeholder. Kind "brief" is the delivery
// brief the chief of staff recorded for a final review, with Brief. Kind
// "recovery" is a recovery card opened for the owner, with Recovery. At is when
// the entry first became observable; sessions and final check attempts use
// their recorded start time.
type FeedEntry struct {
	Image         *FeedImage          `json:"image,omitempty"`
	Workstream    config.WorkstreamID `json:"workstream,omitempty"`
	Repo          config.ProjectID    `json:"repo,omitempty"`
	QueuedReason  string              `json:"queued_reason,omitempty"`
	Kind          string              `json:"kind"`
	At            time.Time           `json:"at"`
	Turn          string              `json:"turn,omitempty"`
	Text          string              `json:"text,omitempty"`
	State         TurnState           `json:"state,omitempty"`
	Status        *StatusView         `json:"status,omitempty"`
	Session       *FeedSession        `json:"session,omitempty"`
	Transition    *FeedTransition     `json:"transition,omitempty"`
	SessionChecks *FeedSessionChecks  `json:"session_checks,omitempty"`
	UnitChecks    *FeedUnitChecks     `json:"unit_checks,omitempty"`
	FinalChecks   *FeedFinalChecks    `json:"final_checks,omitempty"`
	FinalReview   *FeedFinalReview    `json:"final_review,omitempty"`
	Brief         *FeedBrief          `json:"brief,omitempty"`
	Recovery      *FeedRecovery       `json:"recovery,omitempty"`
}

// ImagePath is the path of the stored image id of workstream stream.
func ImagePath(stream config.WorkstreamID, id string) string {
	return Prefix + "/images/" + string(stream) + "/" + id
}

// FeedBrief is the delivery brief recorded for one final review, the latest
// one when it was replaced. Review, ReviewRevision and Commit identify the
// review it covers. Screenshots are the preview images it names.
type FeedBrief struct {
	Review         int         `json:"review"`
	ReviewRevision int         `json:"review_revision"`
	Commit         string      `json:"commit"`
	Text           string      `json:"text"`
	BeforeYouShip  []string    `json:"before_you_ship,omitempty"`
	Screenshots    []FeedImage `json:"screenshots,omitempty"`
}

// FeedSession is one agent turn. State is running, done, waiting (the turn
// ended with a question), failed or interrupted. EndedAt is null while the
// turn runs. Profile is the profile of its latest attempt. Summary is the
// role's report, or the start of its final response when it reported none;
// Failure is why a failed turn failed.
type FeedSession struct {
	Agent     string     `json:"agent"`
	Role      string     `json:"role"`
	Unit      string     `json:"unit,omitempty"`
	Turn      string     `json:"turn"`
	Profile   string     `json:"profile"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	Summary   string     `json:"summary,omitempty"`
	Failure   string     `json:"failure,omitempty"`
}

// FeedTransition identifies the workflow subject that changed, with who
// made it and why. Unit identifies the affected unit when recorded.
// DecidedBy, Judgment and DefaultTo are how the transition router chose the
// transition, present only when it recorded them. Departed is true when Jev
// departed from the default to make the transition, and only then.
type FeedTransition struct {
	Subject   string `json:"subject" description:"Recorded workflow subject, such as feature, unit-upload, unit-upload-checks, final-review or question_1."`
	Unit      string `json:"unit,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
	DecidedBy string `json:"decided_by,omitempty" enum:"code,jev,fallback,owner,jev_gate,reviewer" description:"Who decided the transition when the transition router chose it: code (the default), jev (a departure from the default), fallback (Jev could not answer and the default was committed), owner, jev_gate (one of Jev's gate decisions) or reviewer (a reviewer's verdict authorised it)."`
	Judgment  string `json:"judgment,omitempty" description:"The Jev judgment record behind the decision, when there is one."`
	DefaultTo string `json:"default_to,omitempty" description:"The state the default would have entered."`
	Departed  bool   `json:"departed,omitempty" description:"True when Jev departed from the default to make the transition."`
}

// FeedCheckSelection summarizes the durable selection without check output.
// Selected is the count of chosen links; Judged is the post-narrowing count
// Jev was asked about. Listed is omitted when the trace has no listed total.
type FeedCheckSelection struct {
	FallbackPicks int    `json:"fallback_picks,omitempty"`
	Description   string `json:"description" description:"Sentence describing which checks ran and why."`
	Mode          string `json:"mode" enum:"selected,full"`
	Selected      int    `json:"selected"`
	Judged        int    `json:"judged"`
	Listed        *int   `json:"listed,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// FeedUnitChecks is one run, observable once its selection is recorded.
// Only a recorded result can change running to a terminal state.
type FeedUnitChecks struct {
	TraceURL   string              `json:"trace_url,omitempty" description:"Dagger Cloud trace link of the run; taken from the result record, otherwise from the run's start record."`
	Unit       string              `json:"unit"`
	Run        int                 `json:"run"`
	State      string              `json:"state" enum:"running,passed,failed,incomplete,superseded"`
	Selection  *FeedCheckSelection `json:"selection,omitempty" description:"Absent for historical runs that did not record a selection."`
	Failed     []string            `json:"failed,omitempty" description:"Links of failed checks; captured output stays in the trace."`
	Flaky      []string            `json:"flaky,omitempty" description:"Links of checks that failed and then passed on their rerun."`
	Error      string              `json:"error,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
}

// CheckSourcePullRequest is the source of a final check result read from the
// Dagger Cloud statuses of the workstream's pull request. A final check
// result without a source was produced by a local check run.
const CheckSourcePullRequest = "pull_request"

// CheckCounts are the check counts of a Dagger Cloud run, taken from its
// per-check commit statuses. Cancelled counts the checks the run cancelled.
type CheckCounts struct {
	Total     int `json:"total"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// CheckFailure is one check a Dagger Cloud run reported failed. TraceID is the
// `dagger trace` id its status names, when it names one, and StatusID the id
// of the failing commit status. TargetURL is the Dagger Cloud link that status
// carries, when it carries one.
type CheckFailure struct {
	Name      string `json:"name"`
	TraceID   string `json:"trace_id,omitempty"`
	StatusID  int64  `json:"status_id,omitempty"`
	TargetURL string `json:"target_url,omitempty"`
}

// CheckStatusRef identifies one posting of a commit status. Two postings of
// the same status on one commit have different IDs.
type CheckStatusRef struct {
	ID        int64     `json:"id"`
	State     string    `json:"state,omitempty"`
	TargetURL string    `json:"target_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CheckRerun is the one rerun of a push's failed checks. Original is the
// failing "Run Dagger Checks" posting that asked for it, and Failed the checks
// that failed in it. Fresh is the posting that settled the rerun, once one has,
// and Flakes the originally failed checks the rerun's evidence shows passed.
// Error says why the request itself failed, when it did.
type CheckRerun struct {
	RequestedAt time.Time       `json:"requested_at"`
	Original    CheckStatusRef  `json:"original"`
	Failed      []CheckFailure  `json:"failed,omitempty"`
	Fresh       *CheckStatusRef `json:"fresh,omitempty"`
	Flakes      []string        `json:"flakes,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// The owner's answers to a checks-choice entry.
const (
	ChecksChoiceKeepWaiting = "keep_waiting"
	ChecksChoiceContinue    = "continue"
)

// ChecksChoiceRequest is the owner's answer to a checks-choice inbox entry:
// keep waiting for the check results of a push, or continue its final review
// without them. The fields but Decision are the entry's answer body, which
// pin the push it concerns: its number, pull request, pushed commit, the
// operation that recorded the push and the revision of the offer. An answer
// whose pin is not the workstream's current push and open offer is refused as
// a conflict.
type ChecksChoiceRequest struct {
	Push         int    `json:"push"`
	PullRequest  int    `json:"pull_request"`
	PushedCommit string `json:"pushed_commit"`
	Operation    string `json:"operation"`
	Offer        int    `json:"offer"`
	Decision     string `json:"decision" enum:"keep_waiting,continue"`
}

// ChecksChoiceResponse reports a recorded answer to a checks-choice entry.
type ChecksChoiceResponse struct {
	Project      config.ProjectID    `json:"project"`
	Workstream   config.WorkstreamID `json:"workstream"`
	Push         int                 `json:"push"`
	PullRequest  int                 `json:"pull_request"`
	PushedCommit string              `json:"pushed_commit"`
	Decision     string              `json:"decision"`
	Detail       string              `json:"detail"`
	At           time.Time           `json:"at"`
}

// FeedFinalChecks is one check attempt for a final review.
// Attempt and StartedAt are absent without durable attempt start evidence;
// such entries describe recorded results only, never running checks.
// A result read from the pull request's Dagger Cloud statuses has Source
// "pull_request" and names the pull request, the push whose commit it
// covers, the status the overall result came from, the counts, the failed
// checks with their trace ids and the reruns; Failed then lists the failed
// check names. Results of local runs have none of these.
type FeedFinalChecks struct {
	TraceURL     string              `json:"trace_url,omitempty" description:"Dagger Cloud trace link of the run; taken from the result record, otherwise from the run's start record."`
	Selection    *FeedCheckSelection `json:"selection,omitempty" description:"Absent for historical runs and failures before selection."`
	Review       int                 `json:"review"`
	Attempt      int                 `json:"attempt,omitempty"`
	State        string              `json:"state" enum:"running,passed,failed,incomplete,continued"`
	Source       string              `json:"source,omitempty" enum:"pull_request" description:"Absent for results of local check runs."`
	PullRequest  int                 `json:"pull_request,omitempty"`
	Push         int                 `json:"push,omitempty"`
	StatusURL    string              `json:"status_url,omitempty"`
	Counts       *CheckCounts        `json:"counts,omitempty"`
	FailedChecks []CheckFailure      `json:"failed_checks,omitempty"`
	Reruns       []CheckRerun        `json:"reruns,omitempty"`
	Failed       []string            `json:"failed,omitempty"`
	Flaky        []string            `json:"flaky,omitempty" description:"Links of checks that failed and then passed on their rerun."`
	Error        string              `json:"error,omitempty"`
	StartedAt    *time.Time          `json:"started_at,omitempty"`
	FinishedAt   *time.Time          `json:"finished_at,omitempty"`
}

// FeedSessionChecks summarizes one exploratory session check run: a finished
// result, or the unfinished evidence of a run that printed a trace link and
// has recorded no result.
type FeedSessionChecks struct {
	Run        int                 `json:"run"`
	Commit     string              `json:"commit"`
	Kind       string              `json:"kind"`
	State      string              `json:"state" enum:"running,passed,failed,incomplete" description:"running marks a run with no recorded result: it is still going or was interrupted before it recorded one."`
	Selection  *FeedCheckSelection `json:"selection,omitempty"`
	Failed     []string            `json:"failed,omitempty"`
	Flaky      []string            `json:"flaky,omitempty" description:"Links of checks that failed and then passed on their rerun."`
	Error      string              `json:"error,omitempty"`
	TraceURL   string              `json:"trace_url,omitempty" description:"Dagger Cloud trace link of the run; taken from the result record, otherwise from the run's unfinished evidence."`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty" description:"Absent while the run has no recorded result."`
}

// FeedFinalReview is a recorded review outcome. Reader and Turn identify its
// session entry. Blocking counts blocking findings; Gaps counts uncovered
// criteria, separately from findings.
type FeedFinalReview struct {
	Review   int    `json:"review"`
	State    string `json:"state" enum:"reviewed,failed"`
	Reader   string `json:"reader,omitempty"`
	Turn     string `json:"turn,omitempty"`
	Blocking int    `json:"blocking"`
	Gaps     int    `json:"gaps"`
	Failure  string `json:"failure,omitempty"`
}

// EventKind names the view an event of the client event stream announces a
// change to.
type EventKind string

const (
	// EventResync asks the client to read every view again. It is the first
	// event of every stream and replaces the events a subscriber could not
	// keep up with.
	EventResync       EventKind = "resync"
	EventWorkstream   EventKind = "workstream"
	EventInbox        EventKind = "inbox"
	EventConversation EventKind = "conversation"
	EventImage        EventKind = "image"
	EventRuntime      EventKind = "runtime"
	EventConfig       EventKind = "config"
	EventSpend        EventKind = "spend"
	// EventActivity announces a change to the backend activity record: an
	// operation started, reported progress, finished or failed.
	EventActivity EventKind = "activity"
	// EventCapacity announces a change to the role slots in use or the turns
	// waiting for one, as the projects' schedulers publish them.
	EventCapacity EventKind = "capacity"
	// EventClimbAttemptStarted announces that a Hill Climb workstream began an
	// attempt.
	EventClimbAttemptStarted EventKind = "climb.attempt-started"
	// EventClimbPhase announces that the running attempt moved to another
	// phase: the mason working, the guardrails, a measurement run or the
	// review.
	EventClimbPhase EventKind = "climb.phase"
	// EventClimbAttemptRecorded announces that an attempt's terminal record,
	// climb/attempts/<n>.json, was written.
	EventClimbAttemptRecorded EventKind = "climb.attempt-recorded"
	// EventClimbKept announces that an attempt was kept and became the best.
	EventClimbKept EventKind = "climb.kept"
	// EventClimbPlateau announces that the climb reached its plateau rule.
	EventClimbPlateau EventKind = "climb.plateau"
	// EventClimbStop announces that a stop rule fired and the climb waits at
	// its climb.stop exit.
	EventClimbStop EventKind = "climb.stop"
)

// ClimbEventKinds lists the event kinds a Hill Climb workstream publishes, in
// the order an attempt raises them.
var ClimbEventKinds = []EventKind{EventClimbAttemptStarted, EventClimbPhase, EventClimbAttemptRecorded, EventClimbKept, EventClimbPlateau, EventClimbStop}

// Event is one notification of the client event stream. It names what
// changed, never its new state, which the read endpoints report. Image
// notifications ask clients to refresh the corresponding feed.
type Event struct {
	Kind       EventKind           `json:"kind"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
}

// EngineStatus reports the configured Dagger engine and its probes.
type EngineStatus = engine.Status
