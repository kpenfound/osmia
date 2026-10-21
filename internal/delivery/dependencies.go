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
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// Dependencies supplies the capabilities used from assembly through publication.
// The service owns the repository handles, execution settings and credentials.
type Dependencies struct {
	Now                         func() time.Time
	About                       func(*trace.Repository) *config.Config
	Current                     func() *config.Config
	Effective                   func() (runtime.State, []runtime.Diagnostic)
	ConversationTrace           func(string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *api.APIError)
	Execution                   func() *execution.Controller
	ResolveBase                 func(context.Context, *config.Config, *trace.Repository, config.WorkstreamID, workspace.Provider) (seal.Base, error)
	EnsureCommittee             func(context.Context, *trace.Repository, config.WorkstreamID, int) error
	TrackTurn                   func(config.WorkstreamID, context.CancelFunc) func()
	AgentProfile                func(*config.Config, string, string) (coreadapter.Profile, error)
	RoleExecution               func(*config.Config, string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	ContextFor                  func(*trace.Repository) bundle.Provider
	Stoppable                   func(context.Context, config.ProjectID, config.WorkstreamID) (context.Context, func())
	ThreadRunner                func(*config.Config, *trace.Repository, coreadapter.Turns, func() time.Time) thread.Runner
	Held                        func(*trace.Repository, config.WorkstreamID) error
	ContinueStopped             func(context.Context, *trace.Repository, *config.Config, string, trace.QueuedTurn) (string, error)
	MemoryTurns                 func(*isolation.Turns, *config.Config, *trace.Repository) *isolation.Turns
	Step                        func(string) error
	Committee                   *intake.Committee
	Jev                         *jev.Judge
	AcquireReviewCheckSlot      func(context.Context, config.ProjectID, config.WorkstreamID, int, string) (func(), error)
	Checks                      checks.ReviewChecks
	PullRequests                pulls.Client
	ChangeHeld                  func(*trace.Repository, config.WorkstreamID) (bool, error)
	ChangeRepositoryHeld        func(*trace.Repository, config.WorkstreamID, coreadapter.Operation) (bool, error)
	UpstreamDeliveryPending     func(*trace.Repository, *config.Config, config.WorkstreamID) (bool, error)
	UnresolvedWork              func(*trace.Repository, config.WorkstreamID) (bool, error)
	ReopenHeld                  func(*trace.Repository, config.WorkstreamID) (bool, error)
	BaseObservationAt           func(*trace.Repository, config.WorkstreamID) (bool, int, error)
	WorkgroupMergeOrderResolved func(*trace.Repository, config.WorkstreamID) (string, error)
	ReviewMoves                 func(*trace.Repository, config.WorkstreamID) ([]api.WorkstreamMove, error)
	DependentBaseChanged        func(context.Context, *config.Config, *trace.Repository, config.WorkstreamID) (bool, error)
	BranchBase                  func(context.Context, *trace.Repository, config.WorkstreamID, workspace.Provider, string) (seal.Base, error)
	DriftRebaseNotes            func(*trace.Repository, config.WorkstreamID) ([]string, error)
	WorkgroupEdgePending        func(*trace.Repository, config.WorkstreamID) (bool, error)
	PendingMergeOrderReferences func(*trace.Repository, config.WorkstreamID) ([]string, error)
	EdgePublicationWait         func(*trace.Repository, config.WorkstreamID) (string, error)
	UnresolvedMergeOrderEdge    func(*trace.Repository, config.WorkstreamID) (string, bool, error)
	CanonicalPublication        func(api.DeliveryPublication, *config.Config) bool
}

type Controller struct{ Dependencies }

func New(d Dependencies) *Controller { return &Controller{d} }
