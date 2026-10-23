// Package api defines the local API wire types shared by the service and its clients.
package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
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
	CharterEmpty    Code = "charter_empty"
	Forbidden       Code = "forbidden"
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

// HealthResponse reports the API and the backend separately: API is true
// whenever this response is sent at all, since the handler answers it
// without touching the trace or the activity record's lock. Backend is
// "ready" with no blocking work, "not_ready" while startup or project
// recovery is in progress, and "degraded" when a backend operation has
// failed or made no progress for ActivityStallThreshold; Cause then names
// the operation. Ready is kept for existing clients: it is Backend ==
// "ready".
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
)

// TailnetStatus is the state of the tailnet listener: one of the Tailnet
// constants. LoginURL is the address that approves the node, given with
// needs_login when Tailscale supplies one. Reason says why the listener is
// down.
type TailnetStatus struct {
	State    string `json:"state"`
	LoginURL string `json:"login_url,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type Diagnostic struct {
	Field   string `json:"field"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// ConfigResponse contains validated fields only, never raw configuration text.
// Project is the only active project: null until a project is registered and
// while several are active. Projects lists every active project, in
// active_projects order. LastError is the last failed reload, until a reload
// succeeds.
type ConfigResponse struct {
	Root        string         `json:"root"`
	Digest      string         `json:"digest"`
	Effective   *config.Config `json:"effective"`
	Project     *ProjectView   `json:"project"`
	Projects    []ProjectView  `json:"projects,omitempty"`
	Diagnostics []Diagnostic   `json:"diagnostics"`
	LastError   *ReloadError   `json:"last_error,omitempty"`
	// Drift compares the configuration files on disk with the loaded
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

// ConfigDrift lists the top-level configuration file, then each active
// project's, and any other file a reload would refuse. Differs is true when
// any of them is not unchanged.
type ConfigDrift struct {
	Differs bool         `json:"differs"`
	Files   []ConfigFile `json:"files"`
}

// ConfigFile is one configuration file's State against the loaded
// configuration. Project names the project a project file belongs to. Reason
// says why an invalid file fails, as a reload reports it.
type ConfigFile struct {
	Path    string           `json:"path"`
	Project config.ProjectID `json:"project,omitempty"`
	State   string           `json:"state"`
	Reason  string           `json:"reason,omitempty"`
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

// ProjectView names a registered project and its trace.
// Upstream, Fork, Clone, BaseBranch, Trace and Extraction describe the
// project's default repo, the one its own ID names; Repos lists every repo
// of the project, the default repo included, each with its own settings.
type ProjectView struct {
	ID         config.ProjectID `json:"id"`
	Name       string           `json:"name"`
	Upstream   string           `json:"upstream"`
	Fork       string           `json:"fork,omitempty"`
	Clone      string           `json:"clone"`
	BaseBranch string           `json:"base_branch"`
	Trace      string           `json:"trace"`
	Repos      []RepoView       `json:"repos"`
	// CharterState and Extraction are reported by status only.
	CharterState *CharterState    `json:"charter_state,omitempty"`
	Extraction   *ExtractionState `json:"extraction,omitempty"`
}

// RepoView names one repo of a project: its own upstream, optional fork,
// local clone, landing style, trace and knowledge base.
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
	Default    bool             `json:"default"`
	Extraction *ExtractionState `json:"extraction,omitempty"`
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
// workstream. SkipDebate asks for the workstream's debate to be skipped: its
// spec and plan go to the owner's ratification without a committee.
type HandInRequest struct {
	Base       config.WorkstreamID `json:"base,omitempty"`
	Project    config.ProjectID    `json:"project"`
	Key        string              `json:"key"`
	Path       string              `json:"path,omitempty"`
	URL        string              `json:"url,omitempty"`
	Stdin      *string             `json:"stdin,omitempty"`
	SkipDebate bool                `json:"skip_debate,omitempty"`
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
// left out while exactly one project is active.
type DesignOpenRequest struct {
	Project config.ProjectID `json:"project"`
}

// DesignView is one design session: the workstream it lives on, its project
// and when it opened. It carries no spec, plan or feature state: it stays
// outside every factory pass until the owner starts a workgroup from it.
type DesignView struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Project    config.ProjectID    `json:"project"`
	CreatedAt  time.Time           `json:"created_at"`
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

// DesignStartResponse names the workstream a design session's brief started
// a workgroup on. Handed is the path of the recorded brief; Source names the
// design session it came from. The same thread that held the design
// conversation continues as the workgroup's chief-of-staff thread.
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

// ArchiveResponse reports whether a workstream is archived after the
// owner's request.
type ArchiveResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Archived   bool                `json:"archived"`
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
// workstream for a one-workstream workgroup, exactly as today.
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

type ProjectRemoveRequest struct {
	Project config.ProjectID `json:"project"`
}

type ProjectResponse struct {
	Project  ProjectView `json:"project"`
	NextStep string      `json:"next_step"`
}

// RepoAddRequest adds a repo to an existing project: Project must name an
// active project's default repo. Name defaults to the last path segment of
// Upstream. Clone, Upstream, Fork and BaseBranch behave as they do for
// ProjectAddRequest, but describe this one additional repo, not the project
// as a whole.
type RepoAddRequest struct {
	Project    config.ProjectID `json:"project"`
	Name       string           `json:"name,omitempty"`
	Upstream   string           `json:"upstream"`
	Fork       string           `json:"fork,omitempty"`
	Clone      string           `json:"clone"`
	BaseBranch string           `json:"base_branch,omitempty"`
}

type RuntimeResponse struct {
	Effective runtime.State               `json:"effective"`
	Profiles  map[string]EffectiveProfile `json:"profiles"`
	// Projects lists each active project's context mode.
	Projects []ProjectRuntime `json:"projects"`
	// Jev reports the optional Jev boost. Every mode but ready means its
	// judgments fall back.
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
	Roles         []RoleCapacity `json:"roles" description:"Factory-wide mason, reviewer, committee, architect, librarian and checks slot pools, in that order."`
}

// RoleCapacity is one pool's slots: Used includes turns, workflow reservations
// or check runs, and Limit is its configured global capacity. A zero Limit means
// no global limit; architect and librarian each run at most once per workstream.
// Waiting lists work waiting for a slot and never paused work.
type RoleCapacity struct {
	Role    string     `json:"role" description:"Slot pool: mason, reviewer, committee, architect, librarian or checks. The checks pool uses capacity.checks (default 2) across all projects and workstreams."`
	Used    int        `json:"used" description:"Number of slots currently in use."`
	Limit   int        `json:"limit" description:"Configured slot limit."`
	Waiting []SlotWait `json:"waiting" description:"Work waiting for a slot. Check runs identify their project, workstream and unit, final review or session."`
}

// SlotWait identifies a queued turn, a deferred ready unit, or a check run.
// For check runs, exactly one of Unit, Review or Session identifies the kind.
// Reason is capacity when every slot of the pool is taken, priority when
// higher-priority workstreams start a unit first, and workstream-cap when the
// workstream holds capacity.per_workstream agent turns.
type SlotWait struct {
	Project    config.ProjectID    `json:"project,omitempty" description:"Project of a waiting check run."`
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit,omitempty"`
	Review     int                 `json:"review,omitempty" description:"Final review number for a waiting final-review check run."`
	Session    bool                `json:"session,omitempty" description:"True for a waiting session check run; workstream identifies the session."`
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
	// profile, base commit and branch. Title is a session's optional title.
	Kind          string            `json:"kind,omitempty"`
	Profile       string            `json:"profile,omitempty"`
	Base          string            `json:"base,omitempty"`
	Branch        string            `json:"branch,omitempty"`
	Title         string            `json:"title,omitempty"`
	State         *string           `json:"state"`
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
	// TraceDeleted is set after permanent archive cleanup completes.
	TraceDeleted bool `json:"trace_deleted,omitempty"`
	// DeliveryLink is, for an archived workstream and where one exists, the
	// pull request URL or the recorded upstream commit of an owner-recorded
	// merge.
	DeliveryLink string `json:"delivery_link,omitempty"`
	// Wait names, in plain words, an edge or merge the workstream waits on,
	// for example "waiting on owner: repo-a#123 merge". It is "" while
	// nothing is waited on. A wait is status only: it never creates an
	// inbox entry or a notification (spec#15).
	Wait string `json:"wait,omitempty"`
}

// WorkgroupView presents recorded membership and the workstream graph. It
// carries no workflow authority; the service's existing gates govern members.
type WorkgroupView struct {
	ID      config.WorkgroupID    `json:"id"`
	Members []config.WorkstreamID `json:"members"`
	Edges   []trace.Edge          `json:"edges"`
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
	InboxDrift        = "drift"
	InboxNotices      = "notices"
	InboxLoop         = "loop"
	InboxBase         = "base"
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
// are empty. Project names the project whose workstream
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
}

// InboxAnswer is the existing endpoint that answers an inbox entry, or that
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
}

// FeedResponse lists a workstream's feed, oldest first.
type FeedResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Entries    []FeedEntry         `json:"entries"`
}

// FeedEntry is one event of a workstream's feed. Kind "message", "response"
// and "action" are the conversation with the chief of staff, as
// ConversationEntry describes them, with Turn, Text and State. Kind "status"
// is a status the chief of staff wrote, with Status. Kind "session" is an
// agent turn that started, with Session. Kind "transition" is any recorded
// workflow state change, with Transition. Kind "unit_checks" summarizes a
// unit check run, "final_checks" a final check attempt, and "final_review"
// a final review outcome. At is when the entry first became observable;
// sessions and final check attempts use their recorded start time.
type FeedEntry struct {
	Workstream   config.WorkstreamID `json:"workstream,omitempty"`
	Repo         config.ProjectID    `json:"repo,omitempty"`
	QueuedReason string              `json:"queued_reason,omitempty"`
	Kind         string              `json:"kind"`
	At           time.Time           `json:"at"`
	Turn         string              `json:"turn,omitempty"`
	Text         string              `json:"text,omitempty"`
	State        TurnState           `json:"state,omitempty"`
	Status       *StatusView         `json:"status,omitempty"`
	Session      *FeedSession        `json:"session,omitempty"`
	Transition   *FeedTransition     `json:"transition,omitempty"`
	UnitChecks   *FeedUnitChecks     `json:"unit_checks,omitempty"`
	FinalChecks  *FeedFinalChecks    `json:"final_checks,omitempty"`
	FinalReview  *FeedFinalReview    `json:"final_review,omitempty"`
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
type FeedTransition struct {
	Subject string `json:"subject" description:"Recorded workflow subject, such as feature, unit-upload, unit-upload-checks, final-review or question_1."`
	Unit    string `json:"unit,omitempty"`
	From    string `json:"from"`
	To      string `json:"to"`
	Actor   string `json:"actor"`
	Reason  string `json:"reason"`
}

// FeedCheckSelection summarizes the durable selection without check output.
// Selected is the count of chosen links; Judged is the post-narrowing count
// Jev was asked about. Listed is omitted when the trace has no listed total.
type FeedCheckSelection struct {
	Description string `json:"description" description:"Sentence describing which checks ran and why."`
	Mode        string `json:"mode" enum:"selected,full"`
	Selected    int    `json:"selected"`
	Judged      int    `json:"judged"`
	Listed      *int   `json:"listed,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// FeedUnitChecks is one run, observable once its selection is recorded.
// Only a recorded result can change running to a terminal state.
type FeedUnitChecks struct {
	Unit       string              `json:"unit"`
	Run        int                 `json:"run"`
	State      string              `json:"state" enum:"running,passed,failed,incomplete,superseded"`
	Selection  *FeedCheckSelection `json:"selection,omitempty" description:"Absent for historical runs that did not record a selection."`
	Failed     []string            `json:"failed,omitempty" description:"Links of failed checks; captured output stays in the trace."`
	Error      string              `json:"error,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
}

// FeedFinalChecks is one check attempt for a final review.
// Attempt and StartedAt are absent without durable attempt start evidence;
// such entries describe recorded results only, never running checks.
type FeedFinalChecks struct {
	Selection  *FeedCheckSelection `json:"selection,omitempty" description:"Absent for historical runs and failures before selection."`
	Review     int                 `json:"review"`
	Attempt    int                 `json:"attempt,omitempty"`
	State      string              `json:"state" enum:"running,passed,failed,incomplete"`
	Failed     []string            `json:"failed,omitempty"`
	Error      string              `json:"error,omitempty"`
	StartedAt  *time.Time          `json:"started_at,omitempty"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
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
	EventRuntime      EventKind = "runtime"
	EventConfig       EventKind = "config"
	EventSpend        EventKind = "spend"
	// EventActivity announces a change to the backend activity record: an
	// operation started, reported progress, finished or failed.
	EventActivity EventKind = "activity"
	// EventCapacity announces a change to the role slots in use or the turns
	// waiting for one, as the projects' schedulers publish them.
	EventCapacity EventKind = "capacity"
)

// Event is one notification of the client event stream. It names what
// changed, never its new state, which the read endpoints report.
type Event struct {
	Kind       EventKind           `json:"kind"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
}
