// Package service owns the M1 stores and their local HTTP boundary.
package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
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
// Project is null until a project is registered. LastError is the last failed
// reload, until a reload succeeds.
type ConfigResponse struct {
	Root        string         `json:"root"`
	Digest      string         `json:"digest"`
	Effective   *config.Config `json:"effective"`
	Project     *ProjectView   `json:"project"`
	Diagnostics []Diagnostic   `json:"diagnostics"`
	LastError   *ReloadError   `json:"last_error,omitempty"`
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
	Project    config.ProjectID `json:"project"`
	Key        string           `json:"key"`
	Path       string           `json:"path,omitempty"`
	URL        string           `json:"url,omitempty"`
	Stdin      *string          `json:"stdin,omitempty"`
	SkipDebate bool             `json:"skip_debate,omitempty"`
}

// HandInResponse names the workstream a hand-in created. Handed is the path
// of the copied input; Source is where it came from, as recorded.
// SkipDebate reports that the hand-in skipped the workstream's debate.
type HandInResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Handed     string              `json:"handed"`
	Source     string              `json:"source"`
	SkipDebate bool                `json:"skip_debate,omitempty"`
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
// ratification first, and the chief of staff's recommendation.
type PacketResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Packet     shed.Packet         `json:"packet"`
	Revision   int                 `json:"revision"`
	At         time.Time           `json:"at"`
}

// RatifyRequest ratifies the exact revisions of the spec and the plan the
// owner read in the packet.
type RatifyRequest struct {
	Spec int `json:"spec"`
	Plan int `json:"plan"`
}

// RatifyResponse reports a recorded ratification: the revisions it approves,
// the round it was given in, and the state of its sealing: requested by this
// call, or pending or running from an earlier one.
type RatifyResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Round      int                 `json:"round"`
	Spec       int                 `json:"spec"`
	Plan       int                 `json:"plan"`
	Sealing    string              `json:"sealing"`
	Detail     string              `json:"detail"`
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
	Effective runtime.State               `json:"effective"`
	Profiles  map[string]EffectiveProfile `json:"profiles"`
	// Projects lists each active project's context mode.
	Projects    []ProjectRuntime `json:"projects"`
	Diagnostics []Diagnostic     `json:"diagnostics"`
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
	// FailureStreaks lists the roles and profiles whose latest turn attempts
	// failed with infrastructure failures; it is omitted when there are none.
	FailureStreaks []FailureStreak `json:"failure_streaks,omitempty"`
	Diagnostics    []Diagnostic    `json:"diagnostics"`
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

// WorkstreamStatus is the chief of staff's status for one workstream, next to
// the facts the service owns. State is null until a feature state is
// recorded; Units is empty until the units' states are recorded, and each
// unit carries its latest reported card when present; Advisories holds the
// active overlap advisories about other workstreams of the project; Gates is
// empty while no owner decision waits; Drift is null before the first drift
// rebase; Status is null until the chief of staff writes one. Agents holds
// service-owned facts about claimed and parked turns.
type WorkstreamStatus struct {
	Workstream    config.WorkstreamID `json:"workstream"`
	Project       config.ProjectID    `json:"project"`
	State         *string             `json:"state"`
	Units         []UnitStatus        `json:"units"`
	Advisories    []OverlapAdvisory   `json:"advisories"`
	OpenQuestions int                 `json:"open_questions"`
	Gates         []trace.OwnerGate   `json:"gates"`
	ContextMode   bundle.Mode         `json:"context_mode"`
	Drift         *DriftStatus        `json:"drift"`
	Status        *StatusView         `json:"status"`
	Agents        []AgentStatus       `json:"agents"`
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
)

// InboxEntry is one open owner decision. Kind says which, and Answer how it
// is answered. Number and Batch identify an escalation, the questions the
// chief of staff sent the owner as one ask, and Asked holds its questions as
// their askers put them. Unit names a contested unit, or the unit an
// amendment was filed from; Amendment names an amendment. Revision is the
// revision of the record the decision is taken on. Question is the chief of
// staff's rephrasing of an escalation, and the service's statement of the
// decision for the other kinds.
type InboxEntry struct {
	Kind           string              `json:"kind"`
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

// InboxAnswer is the existing endpoint that answers an inbox entry. Body
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
)

// Event is one notification of the client event stream. It names what
// changed, never its new state, which the read endpoints report.
type Event struct {
	Kind       EventKind           `json:"kind"`
	Project    config.ProjectID    `json:"project,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
}
