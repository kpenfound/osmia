package service

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// TestHooks replaces execution boundaries only for fixture-driven services.
// StartForTest binds these before startup recovery and controller launch.
// Production callers use Start and Options.
type TestHooks struct {
	ConversationTicks <-chan time.Time
	Enforcement       *TestEnforcementBinding
	ReviewChecks      ReviewChecks
	Boundary          func(context.Context, string) error
	SchedulePassed    func()
	NotifyInbox       func(context.Context) (InboxResponse, *APIError)
	CheckJJ           func(context.Context) (string, error)
	SandboxBinary     string
}

// StartForTest starts the real service with fixture execution boundaries.
func StartForTest(ctx context.Context, opts Options, hooks TestHooks) (*Service, error) {
	if hooks.ConversationTicks != nil {
		opts.conversationTicks = hooks.ConversationTicks
	}
	if hooks.Enforcement != nil {
		opts.controls = hooks.Enforcement.controls
	}
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

// TestAccess exposes fixture operations and composed controllers for tests.
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
	sub, ok := a.s.hub.Subscribe()
	if !ok {
		return nil, nil, false
	}
	ticker := time.NewTicker(time.Second)
	return func() error {
		select {
		case <-sub.Wake():
			sub.Take()
		case <-ticker.C:
		case <-a.s.hub.Done():
			return context.Canceled
		}
		return nil
	}, func() { ticker.Stop(); a.s.hub.Unsubscribe(sub) }, true
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

// Intake exposes the composed owner-gate dependencies to controller fixtures.
func (a TestAccess) Intake() *intake.Controller { return a.s.intake() }

// SetTrace replaces a stopped fixture's sole runtime after its trace is reopened.
func (a TestAccess) SetTrace(repo *trace.Repository) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if repo == nil {
		a.s.projects = nil
		return
	}
	a.s.projects = []*activeProject{{id: repo.Project(), repository: repo}}
}

// Handle exercises the local handler without a network transport.
func (a TestAccess) Handle(w http.ResponseWriter, r *http.Request) { a.s.handle(w, r) }

// NewFixtureForTest composes an idle fixture from local state without launching controllers.
func NewFixtureForTest(cfg *config.Config, opts Options, store *runtime.Store, repos []*trace.Repository) *Service {
	s := &Service{cfg: cfg, options: opts, store: store, checkSlots: &checkSlotPool{limit: cfg.Capacity.Checks}}
	for _, r := range repos {
		s.projects = append(s.projects, &activeProject{id: r.Project(), repository: r})
	}
	return s
}

// DoneForTest reports when shutdown finishes.
func (s *Service) DoneForTest() <-chan struct{} { return s.done }

// LifetimeForTest reports the controller lifetime used at recovery boundaries.
func (s *Service) LifetimeForTest() context.Context { return s.lifetime }

// ReadyForTest reports whether startup finished.
func (s *Service) ReadyForTest() bool { return s.ready.Load() }

// ErrForTest reads the shutdown result after DoneForTest closes.
func (s *Service) ErrForTest() error { return s.err }

// TimeoutForTest returns the client's ordinary request budget.
func (c *Client) TimeoutForTest() time.Duration { return c.defaultTimeout }

// SetTimeoutForTest adjusts the fixture's request budget.
func (c *Client) SetTimeoutForTest(d time.Duration) { c.defaultTimeout = d }

func (a TestAccess) Feed(raw string) (FeedResponse, *APIError) { return a.s.feedList(raw) }
func (a TestAccess) Delivery(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	return a.s.deliveryPresentation(ctx, raw)
}
func (a TestAccess) ApproveDelivery(ctx context.Context, raw string, r DeliveryDecision) (DeliveryApproval, *APIError) {
	return a.s.approveDelivery(ctx, raw, r)
}
func (a TestAccess) RuleContested(ctx context.Context, w, u string, r ContestedRulingRequest) (ContestedRulingResponse, *APIError) {
	return a.s.ruleContested(ctx, w, u, r)
}
func (a TestAccess) DispatchHolds(c *config.Config, r *trace.Repository) func(scheduler.Candidate) (bool, error) {
	return a.s.dispatchHolds(c, r)
}
func (a TestAccess) Holds(p config.ProjectID, w config.WorkstreamID, r *trace.Repository, c scheduler.Candidate) (bool, error) {
	return a.s.holds(p, w, r, c)
}
func (a TestAccess) Halt(p config.ProjectID) error {
	_, active := a.s.runtimeOf(p)
	if active == nil {
		return nil
	}
	return a.s.halt(active)
}

type TestPollAttempt = lifecycle.TestPollAttempt

func (a TestAccess) PollAttempts() []TestPollAttempt {
	return a.s.polls.AttemptsForTest()
}

// TestEnforcementBinding retains the runtime binding of fixture enforcement closures.
type TestEnforcementBinding struct{ controls *runtimeControls }

// EnforcementBindingForTest returns the binding created by Enforce. It can
// accompany Threads when a fixture copies that execution boundary alone.
func EnforcementBindingForTest(opts Options) *TestEnforcementBinding {
	return &TestEnforcementBinding{controls: opts.controls}
}

// HoldAnswerDelivery stages an answer-delivery hold for each active project's
// next scheduling pass. Other controllers and conversation turns keep running.
func (a TestAccess) HoldAnswerDelivery() {
	_, projects := a.s.runtimes()
	for _, project := range projects {
		stage := *project.pipeline.current.Load()
		stage.hooks = slices.Clone(stage.hooks)
		for i := range stage.hooks {
			if stage.hooks[i].name == "answers" {
				stage.hooks[i].pass = func(context.Context) error { return nil }
			}
		}
		project.pipeline.next.Store(&stage)
	}
}

// Execution is a test-only seam exposing the composed unit controllers to fixtures.
func (a TestAccess) Execution() *execution.Controller { return a.s.execution() }

func (a TestAccess) Admit(cfg *config.Config, r *trace.Repository) func(context.Context, scheduler.Candidate) (bool, error) {
	return a.s.admit(cfg, r)
}
func (a TestAccess) Capacity(list []WorkstreamStatus) *CapacityStatus {
	return a.s.capacityStatus(list)
}
func (a TestAccess) TraceView(ctx context.Context, w, kind, selector string) (any, *APIError) {
	return a.s.traceView(ctx, w, kind, selector)
}
func (a TestAccess) ContestedEntry(r *trace.Repository, w config.WorkstreamID, u string) (InboxEntry, bool, error) {
	return a.s.contestedEntry(r, w, u)
}

// SetStoppedRuntime binds a reopened runtime store before stopped-controller recovery.
func (a TestAccess) SetStoppedRuntime(store *runtime.Store) error {
	select {
	case <-a.s.done:
		a.s.store = store
		return nil
	default:
		return fmt.Errorf("runtime replacement requires a stopped service")
	}
}

// SetJudge binds a fixture judge for test-controlled check selection.
func (a TestAccess) SetJudge(j *jev.Judge) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.jev = j
	// Final reviewers retain the dependencies composed for their stages.
	for _, p := range a.s.projects {
		if p.pipeline == nil {
			continue
		}
		for _, stage := range []*stages{p.pipeline.current.Load(), p.pipeline.next.Load()} {
			if stage != nil {
				if finals, ok := stage.runner.finals.(*delivery.FinalReviewer); ok {
					finals.S.Jev = j
				}
			}
		}
	}
}

// RecoverWorkspaces restores fixture checkpoints through startup recovery for tests.
func (a TestAccess) RecoverWorkspaces(ctx context.Context, cfg *config.Config) error {
	return a.s.recoverWorkspaces(ctx, cfg)
}

// DeliveryController exposes the composed delivery capabilities to stopped fixtures.
func (a TestAccess) DeliveryController() *delivery.Controller { return a.s.delivery() }

// SetStoppedPullRequests replaces delivery hosting on a stopped fixture.
func (a TestAccess) SetStoppedPullRequests(client pulls.Client) { a.s.options.PullRequests = client }

// Holding reads the same operation holds used by the service reconciler.
func (a TestAccess) Holding(r *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	return a.s.holding(r)
}

// ModelOperationLimit exposes the service's operation admission to stopped fixtures.
func (a TestAccess) ModelOperationLimit(r *trace.Repository, other ...func(coreadapter.Operation) (func(), bool)) func(coreadapter.Operation) (func(), bool) {
	var limit func(coreadapter.Operation) (func(), bool)
	if len(other) > 0 {
		limit = other[0]
	}
	return a.s.modelOperationLimit(r, limit)
}

// LifecycleController exposes the composed lifecycle controller to fixtures.
func (a TestAccess) LifecycleController() *lifecycle.Controller { return a.s.lifecycle() }

// SetPause records an owner pause and announces its runtime change.
func (a TestAccess) SetPause(p runtime.Pause) error { return a.s.setPause(p) }
