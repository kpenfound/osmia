package service

import (
	"context"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// TestHooks replaces execution boundaries only for fixture-driven services.
// StartForTest binds these before startup recovery and controller launch.
// Production callers use Start and Options.
type TestHooks struct {
	ReviewChecks   ReviewChecks
	Boundary       func(context.Context, string) error
	SchedulePassed func()
	NotifyInbox    func(context.Context) (InboxResponse, *APIError)
	CheckJJ        func(context.Context) (string, error)
	SandboxBinary  string
}

// StartForTest starts the real service with fixture execution boundaries.
func StartForTest(ctx context.Context, opts Options, hooks TestHooks) (*Service, error) {
	if hooks.ReviewChecks != nil {
		opts.reviewChecks = hooks.ReviewChecks
	}
	if hooks.Boundary != nil {
		opts.boundary = hooks.Boundary
	}
	if hooks.SchedulePassed != nil {
		opts.schedulePassed = hooks.SchedulePassed
	}
	if hooks.NotifyInbox != nil {
		opts.notifyInbox = hooks.NotifyInbox
	}
	if hooks.CheckJJ != nil {
		opts.checkJJ = hooks.CheckJJ
	}
	if hooks.SandboxBinary != "" {
		opts.sbxBin = hooks.SandboxBinary
	}
	return Start(ctx, opts)
}

// TestAccess exposes fixture operations without exposing controller objects.
type TestAccess struct{ s *Service }

// InspectForTest returns the fixture access for a started service.
func InspectForTest(s *Service) TestAccess { return TestAccess{s: s} }

// Repository returns the service-owned trace. The fixture must not close it.
func (a TestAccess) Repository(id config.ProjectID) (*trace.Repository, error) {
	return a.s.repository(id)
}

// SoleRepository returns the sole active project's trace.
func (a TestAccess) SoleRepository() *trace.Repository {
	_, projects := a.s.runtimes()
	if len(projects) == 0 {
		return nil
	}
	return projects[0].repository
}

// Config returns the loaded configuration for fixture setup.
func (a TestAccess) Config() *config.Config { return a.s.current() }

// Schedule publishes one dispatch pass for a fixture with reconciliation held.
func (a TestAccess) Schedule(ctx context.Context, repo *trace.Repository) error {
	dispatch, err := a.s.dispatcher(a.s.about(repo), repo)
	if err != nil {
		return err
	}
	return dispatch.Pass(ctx)
}

// RefreshRuntime makes a fixture's newly seeded workstreams known to runtime.
func (a TestAccess) RefreshRuntime() error { return a.s.refreshRuntime() }

// Changes subscribes before the caller's first read. The wait must be released.
// Notifications are hints; a timer also wakes reads of unannounced trace state.
func (a TestAccess) Changes() (wait func() error, release func(), ok bool) {
	sub, ok := a.s.hub.subscribe()
	if !ok {
		return nil, nil, false
	}
	ticker := time.NewTicker(time.Second)
	return func() error {
		select {
		case <-sub.wake:
			sub.take()
		case <-ticker.C:
		case <-a.s.hub.done:
			return context.Canceled
		}
		return nil
	}, func() { ticker.Stop(); a.s.hub.unsubscribe(sub) }, true
}

// Now returns the service's configured fixture clock.
func (a TestAccess) Now() time.Time { return a.s.now() }

// LibrarianWorkstreamForTest identifies the service's knowledge extraction trace.
func LibrarianWorkstreamForTest(id config.ProjectID) config.WorkstreamID {
	return librarianWorkstream(id)
}

// EnforceThreadsForTest binds session execution while leaving workflow runners
// under the fixture's control.
func EnforceThreadsForTest(opts Options, e Enforcement) Options {
	enforced := Enforce(opts, e)
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	return opts
}

// TestBuildWorkspace is the service-owned workspace identity of a seeded build.
type TestBuildWorkspace struct {
	Stream     config.WorkstreamID
	At         time.Time
	Branch     string
	Directory  string
	BaseBranch string
}

// PrepareBuild creates the workstream policy and acquires its feature workspace.
// The fixture records the sealed documents and unit graph in that identity.
func (a TestAccess) PrepareBuild(ctx context.Context, repo *trace.Repository, backend, base string) (TestBuildWorkspace, error) {
	cfg := a.s.about(repo)
	stream, err := config.NewWorkstreamID()
	if err != nil {
		return TestBuildWorkspace{}, err
	}
	at := a.s.now()
	if err := repo.CreateWorkstreamOn(ctx, stream, backend, at, ownerActor); err != nil {
		return TestBuildWorkspace{}, err
	}
	if err := recordFactoryPolicy(ctx, repo, stream, at, false); err != nil {
		return TestBuildWorkspace{}, err
	}
	branch := featureBranch(stream)
	w, err := workspaces(cfg, branchesDirectory, backend).Acquire(ctx, vcs.Request{Name: string(stream), Ref: base, Branch: branch})
	if err != nil {
		return TestBuildWorkspace{}, err
	}
	return TestBuildWorkspace{Stream: stream, At: at, Branch: branch, Directory: w.Directory(), BaseBranch: cfg.Project.BaseBranch}, nil
}

// WorkspaceBackend selects workspaces exactly as a new running workstream does.
func (a TestAccess) WorkspaceBackend(ctx context.Context, repo *trace.Repository) (string, error) {
	backend, err := a.s.newWorkspaces(ctx, a.s.about(repo))
	if err != nil {
		return "", err
	}
	return backend.Backend, nil
}

// SetProviderLimit records the provider signal the fixture's backend observed.
func (a TestAccess) SetProviderLimit(limit runtime.ProviderLimit) error {
	return a.s.store.SetProviderLimit(limit)
}

// SessionBranchForTest returns the production branch identity of a session.
func SessionBranchForTest(id config.WorkstreamID) string { return sessionBranch(id) }

// SessionTurnRunning reports whether a session turn is still completing.
func (a TestAccess) SessionTurnRunning(stream config.WorkstreamID) bool {
	a.s.turns.mu.Lock()
	defer a.s.turns.mu.Unlock()
	return len(a.s.turns.streams[stream]) != 0
}

// SessionGit returns the service's session workspace provider for assertions.
func (a TestAccess) SessionGit(project config.ProjectID) *workspace.Git {
	return sessionGit(a.s.current().For(project))
}
