package lifecycle

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type Dependencies struct {
	PollTask                  func(context.Context, *trace.Repository, string, config.WorkstreamID, func(context.Context) error) error
	PollCursor                func(context.Context, string) config.WorkstreamID
	ObserveSharedConversation func(trace.Commit)

	Execution             func() *execution.Controller
	MemoryTurns           func(*isolation.Turns, *config.Config, *trace.Repository) *isolation.Turns
	UnresolvedWork        func(*trace.Repository, config.WorkstreamID) (bool, error)
	LearningsIncorporated func(*trace.Repository, config.WorkstreamID) (bool, error)

	StartActivity     func(key string, kind ActivityKind, project config.ProjectID, stream config.WorkstreamID, phase string) *ActivityHandle
	Abandon           func(ctx context.Context, raw string, req AbandonRequest) (AbandonResponse, *APIError)
	About             func(repository *trace.Repository) *config.Config
	ApprovalTrace     func(ctx context.Context, repo *trace.Repository, id config.WorkgroupID) (*trace.Repository, trace.Workgroup, error)
	ConversationTrace func(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError)
	Current           func() *config.Config
	DriftAsked        *atomic.Bool
	Effective         func() (runtime.State, []runtime.Diagnostic)
	GroupApprovalLock func(id config.WorkgroupID) *sync.Mutex
	Intake            func() *intake.Controller
	Log               func() *slog.Logger
	NewWorkspaces     func(ctx context.Context, cfg *config.Config) (WorkspacesStatus, error)
	Now               func() time.Time
	Options           Execution
	Polls             *PullRequestAttempts
	ProjectFor        func(id config.ProjectID) (config.ProjectID, *APIError)
	Ratify            func(ctx context.Context, raw string, req RatifyRequest) (RatifyResponse, *APIError)
	RecoverThread     func(ctx context.Context, cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID, th trace.Thread) error
	RefreshRuntime    func() error
	Repository        func(id config.ProjectID) (*trace.Repository, error)
	RoleExecution     func(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	RuntimeOf         func(config.ProjectID) (*config.Config, *Project)
	ShedMore          func(ctx context.Context, raw string, req ShedMoreRequest) (ShedResponse, *APIError)
	ShedRedraft       func(ctx context.Context, raw string, req ShedRedraftRequest) (ShedResponse, *APIError)
	Step              func(name string) error
	Stoppable         func(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (context.Context, func())
	Store             func() *runtime.Store
	ThreadRunner      func(cfg *config.Config, store *trace.Repository, turns coreadapter.Turns, now func() time.Time) thread.Runner
}
type Controller struct{ Dependencies }

func New(d Dependencies) *Controller {
	if d.PollTask == nil {
		d.PollTask = func(ctx context.Context, _ *trace.Repository, _ string, _ config.WorkstreamID, poll func(context.Context) error) error {
			return poll(ctx)
		}
	}
	return &Controller{d}
}

type Execution struct {
	Committee    *intake.Committee
	Architect    *intake.Architect
	Threads      func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)
	PullRequests pulls.Client
}
type Project struct {
	ID         config.ProjectID
	Repository *trace.Repository
}
type Controls struct{ Load func() *Controller }
