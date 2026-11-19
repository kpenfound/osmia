package delivery

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// Dependencies supplies the capabilities used from assembly through publication.
// The service owns the repository handles, execution settings and credentials.
type Dependencies struct {
	Now               func() time.Time
	About             func(*trace.Repository) *config.Config
	Current           func() *config.Config
	Effective         func() (runtime.State, []runtime.Diagnostic)
	ConversationTrace func(string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *api.APIError)
	Execution         func() *execution.Controller
	ResolveBase       func(context.Context, *config.Config, *trace.Repository, config.WorkstreamID, workspace.Provider) (seal.Base, error)
	EnsureCommittee   func(context.Context, *trace.Repository, config.WorkstreamID, int) error
	TrackTurn         func(config.WorkstreamID, context.CancelFunc) func()
	AgentProfile      func(*config.Config, string, string) (coreadapter.Profile, error)
	RoleExecution     func(*config.Config, string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	ContextFor        func(*trace.Repository) bundle.Provider
	Stoppable         func(context.Context, config.ProjectID, config.WorkstreamID) (context.Context, func())
	ThreadRunner      func(*config.Config, *trace.Repository, coreadapter.Turns, func() time.Time) thread.Runner
	Held              func(*trace.Repository, config.WorkstreamID) error
	ContinueStopped   func(context.Context, *trace.Repository, *config.Config, string, trace.QueuedTurn) (string, error)
	MemoryTurns       func(*isolation.Turns, *config.Config, *trace.Repository) *isolation.Turns
	Step              func(string) error
	Committee         *intake.Committee
	Jev               *jev.Judge
	// Router applies the delivery decisions the owner gives and routes the exits
	// of the final review's rounds. A reconcile pass that raises an exit hands
	// it to the router's worker.
	Router       router.Port
	PullRequests pulls.Client
	// CloudStatuses reads the combined statuses of the pushed commit. When nil,
	// PullRequests is used if it reads statuses.
	CloudStatuses pulls.StatusReader
	// CloudStatusFetcher reads the JSON a "Run Dagger Checks" status points at.
	CloudStatusFetcher checks.CloudStatusFetcher
	// PostPushWait replaces FinalChecksWait, the time after a push before the
	// statuses of its commit are read. Nil keeps FinalChecksWait.
	PostPushWait *time.Duration
	// CloudRerun requests the one rerun of a push's failed checks. It is nil
	// when none can be requested.
	CloudRerun              checks.CloudRerunner
	ChangeHeld              func(*trace.Repository, config.WorkstreamID) (bool, error)
	ChangeRepositoryHeld    func(*trace.Repository, config.WorkstreamID, coreadapter.Operation) (bool, error)
	UpstreamDeliveryPending func(*trace.Repository, *config.Config, config.WorkstreamID) (bool, error)
	UnresolvedWork          func(*trace.Repository, config.WorkstreamID) (bool, error)
	// RequestRebase asks for the existing rebase of the workstream's feature
	// branch onto its upstream's base branch now, as the owner's request does.
	// It is nil when none can be asked for.
	RequestRebase               func(context.Context, *trace.Repository, config.WorkstreamID) error
	ReopenHeld                  func(*trace.Repository, config.WorkstreamID) (bool, error)
	BaseObservationAt           func(*trace.Repository, config.WorkstreamID) (bool, int, error)
	WorkgroupMergeOrderResolved func(*trace.Repository, config.WorkstreamID) (string, error)
	WorkgroupMergeOrderWaived   func(*trace.Repository, config.WorkstreamID) (string, error)
	ReviewMoves                 func(*trace.Repository, config.WorkstreamID) ([]api.WorkstreamMove, error)
	DependentBaseChanged        func(context.Context, *config.Config, *trace.Repository, config.WorkstreamID) (bool, error)
	BranchBase                  func(context.Context, *trace.Repository, config.WorkstreamID, workspace.Provider, string) (seal.Base, error)
	DriftRebaseNotes            func(*trace.Repository, config.WorkstreamID) ([]string, error)
	WorkgroupEdgePending        func(*trace.Repository, config.WorkstreamID) (bool, error)
	PendingMergeOrderReferences func(*trace.Repository, config.WorkstreamID) ([]string, error)
	EdgePublicationWait         func(*trace.Repository, config.WorkstreamID) (string, error)
	UnresolvedMergeOrderEdge    func(*trace.Repository, config.WorkstreamID) (string, bool, error)
	CanonicalPublication        func(api.DeliveryPublication, *config.Config) bool
	// ChiefThread returns the chief-of-staff thread that carries the
	// workstream's notices and whether a chief turn can run at all. It is nil
	// when none can.
	ChiefThread func(context.Context, *trace.Repository, config.WorkstreamID) (trace.Thread, bool, error)
}

type Controller struct{ Dependencies }

func New(d Dependencies) *Controller { return &Controller{d} }

// postPushWait is how long after a push its statuses are not read.
func (s *Controller) postPushWait() time.Duration {
	if s.PostPushWait != nil {
		return *s.PostPushWait
	}
	return FinalChecksWait
}

// statusReader is the reader of commit statuses the controller uses, or nil
// when it has none.
func (s *Controller) statusReader() pulls.StatusReader {
	if s.CloudStatuses != nil {
		return s.CloudStatuses
	}
	if r, ok := s.PullRequests.(pulls.StatusReader); ok {
		return r
	}
	return nil
}
