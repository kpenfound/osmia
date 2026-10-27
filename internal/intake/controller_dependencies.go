// Package intake owns intake, debate, ratification and owner decision controllers.
package intake

import (
	context "context"
	sync "sync"
	time "time"

	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	workspace "github.com/kpenfound/osmia/internal/workspace"
)

// DebateDependencies contains the boundaries used by the debate controller.
type DebateDependencies struct {
	About             func(repository *trace.Repository) *config.Config
	AgentProfile      func(cfg *config.Config, role string, agent string) (coreadapter.Profile, error)
	BlockMason        func(context.Context, *trace.Repository, config.WorkstreamID, string, string, string) error
	BuildState        func(*trace.Repository, config.WorkstreamID) (BuildState, bool, error)
	ContextFor        func(repo *trace.Repository) bundle.Provider
	ContinueStopped   func(ctx context.Context, repository *trace.Repository, cfg *config.Config, role string, q trace.QueuedTurn) (string, error)
	Current           func() *config.Config
	DeliveryDocuments func(repository *trace.Repository, stream config.WorkstreamID) (trace.Document, trace.Document, *DeliveryApproval, error)
	Held              func(repository *trace.Repository, stream config.WorkstreamID) error
	LatestFinalReport func(repository *trace.Repository, stream config.WorkstreamID) (FinalReport, bool, error)
	MasonBundle       func(context.Context, *trace.Repository, config.WorkstreamID, string) (bundle.Mason, error)
	MemoryTurns       func(turns *isolation.Turns, cfg *config.Config, repo *trace.Repository) *isolation.Turns
	Now               func() time.Time
	Options           Execution
	Pausing           func(project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool)
	RoleExecution     func(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	Step              func(name string) error
	Stoppable         func(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (context.Context, func())
	ThreadRunner      func(cfg *config.Config, store *trace.Repository, turns coreadapter.Turns, now func() time.Time) thread.Runner
	TrackTurn         func(config.WorkstreamID, context.CancelFunc) func()
	drafter           func() *DrafterDependencies
}

func (c *Controller) DebateDependencies() *DebateDependencies {
	return &DebateDependencies{
		About:             c.About,
		AgentProfile:      c.AgentProfile,
		BlockMason:        c.BlockMason,
		BuildState:        c.BuildState,
		ContextFor:        c.ContextFor,
		ContinueStopped:   c.ContinueStopped,
		Current:           c.Current,
		DeliveryDocuments: c.DeliveryDocuments,
		Held:              c.Held,
		LatestFinalReport: c.LatestFinalReport,
		MasonBundle:       c.MasonBundle,
		MemoryTurns:       c.MemoryTurns,
		Now:               c.Now,
		Options:           c.Options,
		Pausing:           c.Pausing,
		RoleExecution:     c.RoleExecution,
		Step:              c.Step,
		Stoppable:         c.Stoppable,
		ThreadRunner:      c.ThreadRunner,
		TrackTurn:         c.TrackTurn,
		drafter:           c.DrafterDependencies,
	}
}

// SealerDependencies contains the boundaries used by the sealer controller.
type SealerDependencies struct {
	About       func(repository *trace.Repository) *config.Config
	Now         func() time.Time
	ResolveBase func(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID, g workspace.Provider) (seal.Base, error)
	Step        func(name string) error
}

func (c *Controller) SealerDependencies() *SealerDependencies {
	return &SealerDependencies{
		About:       c.About,
		Now:         c.Now,
		ResolveBase: c.ResolveBase,
		Step:        c.Step,
	}
}

// BuilderDependencies contains the boundaries used by the builder controller.
type BuilderDependencies struct {
	ChangeHeld           func(repo *trace.Repository, stream config.WorkstreamID) (bool, error)
	ChangeRepositoryHeld func(repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) (bool, error)
	Now                  func() time.Time
	Step                 func(name string) error
}

func (c *Controller) BuilderDependencies() *BuilderDependencies {
	return &BuilderDependencies{
		ChangeHeld:           c.ChangeHeld,
		ChangeRepositoryHeld: c.ChangeRepositoryHeld,
		Now:                  c.Now,
		Step:                 c.Step,
	}
}

// ChartererDependencies contains the boundaries used by the charterer controller.
type ChartererDependencies struct {
	Now  func() time.Time
	Step func(name string) error
}

func (c *Controller) ChartererDependencies() *ChartererDependencies {
	return &ChartererDependencies{
		Now:  c.Now,
		Step: c.Step,
	}
}

// ShedOwnerDependencies contains the boundaries used by the shedowner controller.
type ShedOwnerDependencies struct {
}

func (c *Controller) ShedOwnerDependencies() *ShedOwnerDependencies {
	return &ShedOwnerDependencies{}
}

// HandInDependencies contains the boundaries used by the handin controller.
type HandInDependencies struct {
	RecordOwnerProjects func(context.Context, *trace.Repository, config.WorkstreamID, []config.ProjectID, config.WorkstreamID) *APIError
	About               func(repository *trace.Repository) *config.Config
	ArchivedWorkstream  func(raw string) (runtime.Archive, bool)
	HandInMu            *sync.Mutex
	NewWorkspaces       func(ctx context.Context, cfg *config.Config) (WorkspacesStatus, error)
	RefreshRuntime      func() error
}

func (c *Controller) HandInDependencies() *HandInDependencies {
	return &HandInDependencies{
		RecordOwnerProjects: c.RecordOwnerProjects,
		About:               c.About,
		ArchivedWorkstream:  c.ArchivedWorkstream,
		HandInMu:            c.HandInMu,
		NewWorkspaces:       c.NewWorkspaces,
		RefreshRuntime:      c.RefreshRuntime,
	}
}

// DrafterDependencies contains the boundaries used by the drafter controller.
type DrafterDependencies struct {
	About              func(repository *trace.Repository) *config.Config
	ChangeHeld         func(repo *trace.Repository, stream config.WorkstreamID) (bool, error)
	ContextFor         func(repo *trace.Repository) bundle.Provider
	ContinueStopped    func(ctx context.Context, repository *trace.Repository, cfg *config.Config, role string, q trace.QueuedTurn) (string, error)
	Current            func() *config.Config
	DesignStartPending func(r *trace.Repository, stream config.WorkstreamID) (bool, error)
	FactoryContext     func(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool
	Held               func(repository *trace.Repository, stream config.WorkstreamID) error
	InspectCode        func(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool
	MemoryTurns        func(turns *isolation.Turns, cfg *config.Config, repo *trace.Repository) *isolation.Turns
	Now                func() time.Time
	Options            Execution
	Pausing            func(project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool)
	RemoteFileTool     func(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool
	RoleExecution      func(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error)
	Stoppable          func(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (context.Context, func())
	ThreadRunner       func(cfg *config.Config, store *trace.Repository, turns coreadapter.Turns, now func() time.Time) thread.Runner
	TrackTurn          func(config.WorkstreamID, context.CancelFunc) func()
}

func (c *Controller) DrafterDependencies() *DrafterDependencies {
	return &DrafterDependencies{
		About:              c.About,
		ChangeHeld:         c.ChangeHeld,
		ContextFor:         c.ContextFor,
		ContinueStopped:    c.ContinueStopped,
		Current:            c.Current,
		DesignStartPending: c.DesignStartPending,
		FactoryContext:     c.FactoryContext,
		Held:               c.Held,
		InspectCode:        c.InspectCode,
		MemoryTurns:        c.MemoryTurns,
		Now:                c.Now,
		Options:            c.Options,
		Pausing:            c.Pausing,
		RemoteFileTool:     c.RemoteFileTool,
		RoleExecution:      c.RoleExecution,
		Stoppable:          c.Stoppable,
		ThreadRunner:       c.ThreadRunner,
		TrackTurn:          c.TrackTurn,
	}
}

func (d *DebateDependencies) DrafterDependencies() *DrafterDependencies { return d.drafter() }
