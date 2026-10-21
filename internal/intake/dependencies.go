package intake

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

type Dependencies struct {
	BuildState  func(*trace.Repository, config.WorkstreamID) (BuildState, bool, error)
	MasonBundle func(context.Context, *trace.Repository, config.WorkstreamID, string) (bundle.Mason, error)
	BlockMason  func(context.Context, *trace.Repository, config.WorkstreamID, string, string, string) error

	About                    func(repository *trace.Repository) *config.Config
	ApprovalTrace            func(ctx context.Context, repo *trace.Repository, id config.WorkgroupID) (*trace.Repository, trace.Workgroup, error)
	ArchivedWorkstream       func(raw string) (runtime.Archive, bool)
	BaseEntry                func(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error)
	ChangeHeld               func(repo *trace.Repository, stream config.WorkstreamID) (bool, error)
	ChangeRepositoryHeld     func(repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) (bool, error)
	ChiefContext             func(ctx context.Context, provider bundle.Provider, repository *trace.Repository, closeRepository func(), project config.ProjectID, stream config.WorkstreamID) (string, error)
	ChiefOverride            func(cfg *config.Config) (string, coreadapter.Profile, error)
	ChiefRepository          func(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (target *trace.Repository, host config.WorkstreamID, bundleStream config.WorkstreamID, close func(), err error)
	ContestedEntry           func(repository *trace.Repository, stream config.WorkstreamID, unit string) (InboxEntry, bool, error)
	ContextFor               func(repo *trace.Repository) bundle.Provider
	ContinueStopped          func(ctx context.Context, repository *trace.Repository, cfg *config.Config, role string, q trace.QueuedTurn) (string, error)
	ContractGuardRefusal     func(wt *trace.Repository, g trace.Workgroup, spec trace.ProjectSpec) (string, error)
	ConversationTrace        func(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError)
	CrossRepoMemberRepos     func(ctx context.Context, g trace.Workgroup) ([]trace.CrossRepoMember, error)
	Current                  func() *config.Config
	DeliveryDocuments        func(repository *trace.Repository, stream config.WorkstreamID) (trace.Document, trace.Document, *DeliveryApproval, error)
	DeliveryEntry            func(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error)
	DriftEntry               func(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error)
	Effective                func() (runtime.State, []runtime.Diagnostic)
	FactoryContext           func(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool
	FinalChecksEntry         func(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error)
	FinalRoundsWithBlocking  func(repository *trace.Repository, stream config.WorkstreamID) (int, error)
	FirstLiveWorkgroupMember func(g trace.Workgroup) (config.WorkstreamID, error)
	GroupApprovalLock        func(id config.WorkgroupID) *sync.Mutex
	Held                     func(repository *trace.Repository, stream config.WorkstreamID) error
	InspectCode              func(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool
	LatestFinalReport        func(repository *trace.Repository, stream config.WorkstreamID) (FinalReport, bool, error)
	LoadCharter              func(ctx context.Context, id config.ProjectID) (trace.Document, charter.Charter, error)
	Log                      func() *slog.Logger
	MemberArchives           func() []runtime.Archive
	MemoryTurns              func(turns *isolation.Turns, cfg *config.Config, repo *trace.Repository) *isolation.Turns
	NewWorkspaces            func(ctx context.Context, cfg *config.Config) (WorkspacesStatus, error)
	Now                      func() time.Time
	Pausing                  func(project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool)
	PrepareGroupApproval     func(ctx context.Context, Decision trace.GroupApproval) error
	ProjectFor               func(id config.ProjectID) (config.ProjectID, *APIError)
	PublicationEntry         func(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error)
	RefreshRuntime           func() error
	RemoteFileTool           func(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool
	Repository               func(id config.ProjectID) (*trace.Repository, error)
	ResolveBase              func(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID, g workspace.Provider) (seal.Base, error)
	ResumeGroupApproval      func(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, id config.WorkgroupID, asked shed.Pin, projectSpecRevision int) (RatifyResponse, bool, *APIError)
	RoleExecution            func(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	RuntimeOf                func(id config.ProjectID) (*config.Config, *Project)
	Runtimes                 func() (*config.Config, []*Project)
	Step                     func(name string) error
	Stoppable                func(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (context.Context, func())
	StoreGeneration          func() uint64
	ThreadRunner             func(cfg *config.Config, store *trace.Repository, turns coreadapter.Turns, now func() time.Time) thread.Runner
	WorkgroupFullyRetired    func(ctx context.Context, g trace.Workgroup) (bool, error)
	Store                    func() *runtime.Store
	HandInMu                 *sync.Mutex
	Jev                      *jev.Judge
	TrackTurn                func(config.WorkstreamID, context.CancelFunc) func()
	Options                  Execution
}
type Execution struct {
	Architect *Architect
	Committee *Committee
	Issues    issues.Client
	Controls  *Controls
}
type Controller struct{ Dependencies }

func New(deps Dependencies) *Controller { return &Controller{Dependencies: deps} }

type Controls struct{ Load func() *Controller }
type Project struct {
	ID         config.ProjectID
	Repository *trace.Repository
}
type BuildState struct {
	States map[string]trace.WorkflowState
	Plan   plan.Plan
}
