package journey

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// WorkspaceCleanup runs the lifecycle's workspace cleanup over a project's
// trace. Failures holds what the passes could not clean.
type WorkspaceCleanup struct {
	Cfg        *config.Config
	Repository *trace.Repository
	Now        func() time.Time
	Failures   map[string]lifecycle.CleanupFailure
	core       *lifecycle.WorkspaceCleanup
}

func (c *WorkspaceCleanup) coreCleanup() *lifecycle.WorkspaceCleanup {
	if c.core == nil {
		c.core = &lifecycle.WorkspaceCleanup{}
	}
	c.core.Cfg, c.core.Repository, c.core.Now = c.Cfg, c.Repository, c.Now
	if c.core.Failures == nil {
		c.core.Failures = map[string]lifecycle.CleanupFailure{}
	}
	c.Failures = c.core.Failures
	return c.core
}

// Pass runs one cleanup pass.
func (c *WorkspaceCleanup) Pass(ctx context.Context) error { return c.coreCleanup().Pass(ctx) }

// Busy reports which of the stream's workspaces are in use.
func (c *WorkspaceCleanup) Busy(stream config.WorkstreamID) (map[string]bool, bool, error) {
	return c.coreCleanup().Busy(stream)
}

// ArchiveCleanup runs the lifecycle's archive cleanup for a service.
type ArchiveCleanup struct {
	S          *Service
	Workspaces *WorkspaceCleanup
	Finish     func(config.Root, config.ProjectID, config.WorkstreamID) error
	core       *lifecycle.ArchiveCleanup
}

func (c *ArchiveCleanup) coreCleanup() *lifecycle.ArchiveCleanup {
	if c.core == nil {
		c.core = &lifecycle.ArchiveCleanup{}
	}
	c.core.S = c.S.Lifecycle()
	c.core.Workspaces = c.Workspaces.coreCleanup()
	c.core.Finish = c.Finish
	return c.core
}

// Pass runs one cleanup pass.
func (c *ArchiveCleanup) Pass(ctx context.Context) error { return c.coreCleanup().Pass(ctx) }

// Prepare reports whether the archive is eligible for cleanup and why not.
func (c *ArchiveCleanup) Prepare(ctx context.Context, a runtime.Archive) (eligible bool, reason string, err error) {
	return c.coreCleanup().Prepare(ctx, a)
}

// Lifecycle returns the lifecycle controls bound to the running service.
func (c *RuntimeControls) Lifecycle() *lifecycle.Controls {
	return &lifecycle.Controls{Load: func() *lifecycle.Controller {
		if c == nil || c.Service.Load() == nil {
			return nil
		}
		return c.Service.Load().Lifecycle()
	}}
}

// DriftDriver returns the lifecycle's drift foreman over the same masons,
// carrying the foreman's next scheduled drift.
func (f *Foreman) DriftDriver() *lifecycle.Foreman {
	d := lifecycle.NewForeman(f.S.Lifecycle(), f.Cfg, f.Repository)
	d.NextDrift = f.NextDrift
	return d
}

// Drifts runs the drift pass and keeps the next scheduled drift it reports.
func (f *Foreman) Drifts(ctx context.Context) error {
	driver := f.DriftDriver()
	err := driver.Drifts(ctx)
	f.NextDrift = driver.NextDrift
	return err
}

// Notifier drives a service's owner notifications one pass at a time.
type Notifier struct{ driver *service.TestNotifier }

// NewNotifier returns a notifier for s that retries after retry.
func NewNotifier(s *Service, retry time.Duration) *Notifier {
	return &Notifier{driver: service.NotifierForTest(s.Core(), retry)}
}

// Pass runs one notification pass.
func (n *Notifier) Pass(ctx context.Context) { n.driver.Pass(ctx) }

// Diagnostics lists what the notifier reports as wrong with its webhook.
func (n *Notifier) Diagnostics() []service.Diagnostic { return n.driver.Diagnostics() }

// DeltaRunner runs workgroup delta turns for a service's lifecycle.
type DeltaRunner struct{ S *Service }

// Load reads the change a delta turn belongs to.
func (d DeltaRunner) Load(ctx context.Context, in lifecycle.DeltaTurnInput) (*trace.Repository, lifecycle.WorkgroupChange, bool, func(), error) {
	return lifecycle.DeltaRunner{S: d.S.Lifecycle()}.Load(ctx, in)
}

// Inspect reports the state of the delta turn's operation.
func (d DeltaRunner) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return lifecycle.DeltaRunner{S: d.S.Lifecycle()}.Inspect(ctx, op)
}

// Apply runs the delta turn's operation.
func (d DeltaRunner) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return lifecycle.DeltaRunner{S: d.S.Lifecycle()}.Apply(ctx, op)
}

// WorkgroupResolver runs the lifecycle's workgroup resolution pass over a
// repository.
type WorkgroupResolver struct {
	S          *Service
	Repository *trace.Repository
}

// Pass runs one resolution pass.
func (w *WorkgroupResolver) Pass(ctx context.Context) error {
	return (&lifecycle.WorkgroupResolver{S: w.S.Lifecycle(), Repository: w.Repository}).Pass(ctx)
}
