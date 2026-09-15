// Package coreadapter defines Osmia's execution ports. Implementations may use
// busybees/core; callers own workflow policy and durable records.
package coreadapter

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrUnsupported means a requested capability cannot be provided. Adapters must
// return it before launching work rather than silently weakening a boundary.
var ErrUnsupported = errors.New("unsupported execution capability")

// Scope identifies caller-owned records; none of these values imply readiness.
type Scope struct{ Project, Workstream, Unit, Thread, Turn, Role string }

type Access string

const (
	ReadOnly  Access = "read-only"
	ReadWrite Access = "read-write"
)

type Workspace struct {
	ID, Directory, Revision string
	Access                  Access
}
type WorkspaceRequest struct {
	Scope                                    Scope
	SourceDirectory, BaseRevision, Directory string
	Access                                   Access
}

// Lease remains owned by the service across turns. Release is idempotent; callers
// use a non-cancelled cleanup context after a cancelled operation.
type Lease interface{ Release(context.Context) error }
type WorkspaceLease struct {
	Workspace Workspace
	Lease     Lease
}
type Workspaces interface {
	Acquire(context.Context, WorkspaceRequest) (WorkspaceLease, error)
}

// Capabilities are an allowlist selected by the service for this role. An empty
// list grants nothing. Repository configuration cannot expand these permissions.
type Capabilities struct {
	Tools                        []string
	WriteFiles, Execute, Network bool
}

// Isolation describes required enforcement, not profile hints. Environment is
// the complete public environment, never an overlay on inherited host variables.
// Credentials contain provider/MCP secret references, never secret values;
// delivery credentials must not be referenced. VCS tools, writable metadata and
// inherited credentials must all be denied, including through repository tools.
type Isolation struct {
	Workspace                                                  Workspace
	Capabilities                                               Capabilities
	Environment                                                map[string]string
	Credentials                                                []CredentialRef
	DenyVCS, DenyInheritedEnvironment, DenyDeliveryCredentials bool
}
type CredentialRef struct{ EnvironmentName, Reference string }
type SandboxRequest struct {
	Scope    Scope
	Mode     string
	Required Isolation
}

// SandboxLease records the boundary actually verified by the implementation.
// Verified must satisfy Required before any turn starts; a fake is no evidence
// that a host or container enforces this boundary.
type SandboxLease struct {
	ID       string
	Verified Isolation
	Lease    Lease
}
type Sandboxes interface {
	Prepare(context.Context, SandboxRequest) (SandboxLease, error)
}

type Tool struct {
	Name, Description string
	InputSchema       json.RawMessage
	Handle            func(context.Context, json.RawMessage) (json.RawMessage, error)
}
type HostRequest struct {
	Scope        Scope
	Capabilities Capabilities
	Tools        []Tool
}
type Endpoint struct{ URL, BearerTokenEnvironment string }
type HostedMCP struct {
	Endpoint Endpoint
	Lease    Lease
}

// MCPHosts exposes only the supplied role-scoped tools. It owns transport
// lifetime, not handlers' durable effects or routing decisions.
type MCPHosts interface {
	Host(context.Context, HostRequest) (HostedMCP, error)
}

type Profile struct {
	Name, Backend, Model, Effort string
	Timeout                      time.Duration
	MaxTurns                     int
	CostLimitUSD                 float64
}
type BackendSession struct{ Backend, ID string }

// PreparedTurn contains caller-prepared context, resources and outcome policy.
// History is rendered from the owned log when backend resume is unavailable.
// AllowedOutcomes is an allowlist: an empty list accepts no reported status.
type PreparedTurn struct {
	Scope                                           Scope
	Profile                                         Profile
	Sandbox                                         SandboxLease
	MCP                                             []Endpoint
	SessionDirectory, SystemPrompt, Prompt, History string
	Resume                                          *BackendSession
	AllowedOutcomes                                 []string
}
type Outcome struct{ Status, Report string }
type Usage struct {
	CostUSD   float64
	CostKnown bool
	Turns     int
}
type SessionResult struct {
	Session                         BackendSession
	SessionDirectory, FinalResponse string
	StartedAt                       time.Time
	Duration                        time.Duration
	ExitCode, Signal                int
	TimedOut, Cancelled, IsError    bool
	ErrorSubtype                    string
	Outcome                         *Outcome
	Usage                           Usage
	Limit                           *ProviderLimit
}
type ProviderLimit struct {
	Status, Kind string
	ResetsAt     time.Time
}

// Turns returns partial results alongside errors when available. A reported
// outcome is data for Osmia, never permission for an adapter to transition state.
type Turns interface {
	Run(context.Context, PreparedTurn) (SessionResult, error)
}

type Candidate struct{ Revision, BaseRevision, SpecRevision, PlanRevision string }
type ReviewRequest struct {
	Turn                    PreparedTurn
	Candidate               Candidate
	Diff, ArtifactDirectory string
	Context                 []ContextItem
	Angles                  []string
}
type ContextItem struct{ Source, Content, SkippedReason string }
type Finding struct {
	Category, Severity, Path, Side, Summary, Evidence string
	StartLine, EndLine                                int
}
type Artifact struct{ Kind, Path string }
type ReviewResult struct {
	Candidate Candidate
	Verdict   string
	Findings  []Finding
	Artifacts []Artifact
	Sessions  []SessionResult
	Partial   bool
}

// Reviews produces evidence tied to an exact candidate; approval and landing
// remain service decisions. Partial artifacts may accompany an error.
type Reviews interface {
	Review(context.Context, ReviewRequest) (ReviewResult, error)
}

type FailureKind string

const (
	Infrastructure FailureKind = "infrastructure"
	Behavioural    FailureKind = "behavioural"
)

type RetryRequest struct {
	Result              SessionResult
	Err                 error
	Attempt, MaxRetries int
	Delay               time.Duration
	FallbackProfile     string
}
type RetryDecision struct {
	Kind            FailureKind
	Reason          string
	Retry           bool
	Delay           time.Duration
	FallbackProfile string
}

// Retries classifies one completed attempt (one-based) using caller-supplied
// limits. It neither sleeps, launches retries nor changes profile bindings.
type Retries interface {
	Decide(context.Context, RetryRequest) (RetryDecision, error)
}

type LedgerEntry struct {
	Scope     Scope
	AttemptID string
	At        time.Time
	Usage     Usage
}
type SpendQuery struct {
	Workstreams []string
	Since       time.Time
}
type Spend struct {
	CostUSD      float64
	UnknownCosts int
}

// Ledger stores accounting only. The caller reconciles AttemptID before retrying
// an append; this port makes no exactly-once or state-transaction guarantee.
type Ledger interface {
	Append(context.Context, LedgerEntry) error
	Spend(context.Context, SpendQuery) (Spend, error)
}
type BudgetRequest struct {
	Spend                   Spend
	LimitUSD, ResumePercent float64
	PreviouslyReached       bool
}
type BudgetResult struct {
	Reached, Crossed, Released bool
	UnknownCosts               int
}

// Budgets reports threshold signals; it does not pause or resume dispatch.
type Budgets interface {
	Evaluate(context.Context, BudgetRequest) (BudgetResult, error)
}

type ClaimRequest struct {
	Scope Scope
	Slots map[string]int
}
type Claim struct {
	Acquired bool
	Lease    Lease
}

// Capacity attempts an all-or-none, nonblocking claim. An unavailable claim is
// Acquired=false with no error. The caller chooses ordering and all slot limits.
type Capacity interface {
	TryClaim(context.Context, ClaimRequest) (Claim, error)
}

// Wakeups is a per-controller, coalescing latency hint. Wait returns on a wake,
// the caller's tick or context cancellation. It is not a durable event queue.
type Wakeups interface {
	Notify(context.Context) error
	Wait(context.Context, <-chan time.Time) error
}
