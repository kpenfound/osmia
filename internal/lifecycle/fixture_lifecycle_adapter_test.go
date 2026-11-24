package lifecycle_test

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

type archiveCleanup struct {
	s          *Service
	workspaces *workspaceCleanup

	finish func(config.Root, config.ProjectID, config.WorkstreamID) error
	_core  *lifecycle.ArchiveCleanup
}

func (c *archiveCleanup) Pass(ctx context.Context) error {
	return (c.coreCleanup()).Pass(ctx)
}

func (c *archiveCleanup) prepare(ctx context.Context, a runtime.Archive) (eligible bool, reason string, err error) {
	return (c.coreCleanup()).Prepare(ctx, a)
}

type baseRefresher struct {
	s          *Service
	repository *trace.Repository
}

func (b *baseRefresher) Pass(ctx context.Context) error {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).Pass(ctx)
}

func (b *baseRefresher) ObserveDraftPullRequests(ctx context.Context) error {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).ObserveDraftPullRequests(ctx)
}

func (b *baseRefresher) ObserveLandedPullRequests(ctx context.Context) error {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).ObserveLandedPullRequests(ctx)
}

func (b *baseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).Inspect(ctx, op)
}
func (b *baseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).Apply(ctx, op)
}

type workspaceCleanup struct {
	cfg        *config.Config
	repository *trace.Repository
	now        func() time.Time
	failures   map[string]lifecycle.CleanupFailure
	_core      *lifecycle.WorkspaceCleanup
}

func (c *workspaceCleanup) Pass(ctx context.Context) error {
	return (c.coreCleanup()).Pass(ctx)
}

func (c *workspaceCleanup) busy(stream config.WorkstreamID) (map[string]bool, bool, error) {
	return (c.coreCleanup()).Busy(stream)
}

func (f *foreman) requestDrift(ctx context.Context, stream config.WorkstreamID, why string) error {
	return (f.driftDriver()).RequestDrift(ctx, stream, why)
}

type drifter struct{ *foreman }

func (d drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).Inspect(ctx, op)
}
func (d drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).Apply(ctx, op)
}

type resolutions struct{ intake.StreamWorkspaces }

func (r resolutions) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return (lifecycle.Resolutions{StreamWorkspaces: r.StreamWorkspaces}).Acquire(ctx, req)
}
func (r resolutions) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return (lifecycle.Resolutions{StreamWorkspaces: r.StreamWorkspaces}).Selection(ctx, scope, execution)
}
func (r resolutions) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView) error {
	return (lifecycle.Resolutions{StreamWorkspaces: r.StreamWorkspaces}).Capture(ctx, scope, view)
}

func (d drifter) resolvePrompt(ctx context.Context, stream config.WorkstreamID, rebase lifecycle.DriftRebase) (string, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).ResolvePrompt(ctx, stream, rebase)
}
func (d drifter) fixPrompt(stream config.WorkstreamID, rebase lifecycle.DriftRebase) (string, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).FixPrompt(stream, rebase)
}
func (d drifter) reviewPrompt(ctx context.Context, stream config.WorkstreamID, rebase lifecycle.DriftRebase) (string, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).ReviewPrompt(ctx, stream, rebase)
}

func (f *foreman) driftDue(stream config.WorkstreamID, now time.Time) (string, error) {
	return (f.driftDriver()).DriftDue(stream, now)
}
func (f *foreman) drifts(ctx context.Context) error {
	driver := f.driftDriver()
	err := driver.Drifts(ctx)
	f.nextDrift = driver.NextDrift
	return err
}
func (f *foreman) askDrift(ctx context.Context, stream config.WorkstreamID, at time.Time) (int, error) {
	return (f.driftDriver()).AskDrift(ctx, stream, at)
}

type deltaRunner struct{ s *Service }

func (d deltaRunner) load(ctx context.Context, in lifecycle.DeltaTurnInput) (*trace.Repository, lifecycle.WorkgroupChange, bool, func(), error) {
	return (lifecycle.DeltaRunner{S: d.s.lifecycle()}).Load(ctx, in)
}
func (d deltaRunner) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return (lifecycle.DeltaRunner{S: d.s.lifecycle()}).Inspect(ctx, op)
}
func (d deltaRunner) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return (lifecycle.DeltaRunner{S: d.s.lifecycle()}).Apply(ctx, op)
}

type workgroupResolver struct {
	s          *Service
	repository *trace.Repository
}

func (w *workgroupResolver) Pass(ctx context.Context) error {
	return (&lifecycle.WorkgroupResolver{S: w.s.lifecycle(), Repository: w.repository}).Pass(ctx)
}
func (s *Service) lifecycle() *lifecycle.Controller {
	c := service.InspectForTest(s.core()).LifecycleController()
	c.Options.Architect = s.options.Architect
	c.Options.Committee = s.options.Committee
	c.Options.PullRequests = s.options.PullRequests
	c.Execution = s.execution
	c.Intake = s.intake
	if s.options.Reconciliation.Now != nil {
		c.Now = s.options.Reconciliation.Now
	}
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}
func (c *runtimeControls) lifecycle() *lifecycle.Controls {
	return &lifecycle.Controls{Load: func() *lifecycle.Controller {
		if c == nil || c.service.Load() == nil {
			return nil
		}
		return c.service.Load().lifecycle()
	}}
}
func (f *foreman) driftDriver() *lifecycle.Foreman {
	d := lifecycle.NewForeman(f.s.lifecycle(), f.cfg, f.repository)
	d.NextDrift = f.nextDrift
	return d
}
func (c *workspaceCleanup) coreCleanup() *lifecycle.WorkspaceCleanup {
	if c._core == nil {
		c._core = &lifecycle.WorkspaceCleanup{Cfg: c.cfg, Repository: c.repository, Now: c.now}
	}
	c._core.Cfg = c.cfg
	c._core.Repository = c.repository
	c._core.Now = c.now
	if c._core.Failures == nil {
		c._core.Failures = map[string]lifecycle.CleanupFailure{}
	}
	c.failures = c._core.Failures
	return c._core
}
func (c *archiveCleanup) coreCleanup() *lifecycle.ArchiveCleanup {
	if c._core == nil {
		c._core = &lifecycle.ArchiveCleanup{}
	}
	c._core.S = c.s.lifecycle()
	c._core.Workspaces = c.workspaces.coreCleanup()
	c._core.Finish = c.finish
	return c._core
}
