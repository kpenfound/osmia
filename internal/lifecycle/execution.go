package lifecycle

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/trace"
)

type masons struct {
	s          *Controller
	cfg        *config.Config
	repository *trace.Repository
}

func (m *masons) driver() *execution.Masons {
	return execution.NewMasons(m.s.Execution(), m.cfg, m.repository)
}
func (m *masons) read(w config.WorkstreamID) (execution.Building, bool, error) {
	return m.driver().Read(w)
}
func (m *masons) recoverView(ctx context.Context, w config.WorkstreamID, a string, view func() (string, error)) error {
	return m.driver().RecoverView(ctx, w, a, view)
}

type Foreman struct {
	*masons
	NextDrift time.Time
}

func (f *Foreman) driver() *execution.Foreman { return execution.NewForeman(f.masons.driver()) }
func (f *Foreman) header(id string, w config.WorkstreamID, u, cause string, at time.Time) trace.Header {
	return f.driver().Header(id, w, u, cause, at)
}
func (f *Foreman) requestedAt(w config.WorkstreamID, op string) (time.Time, error) {
	return f.driver().RequestedAt(w, op)
}
func NewForeman(c *Controller, cfg *config.Config, repo *trace.Repository) *Foreman {
	return &Foreman{masons: &masons{s: c, cfg: cfg, repository: repo}}
}
func NewDrifter(c *Controller, cfg *config.Config, repo *trace.Repository) Drifter {
	return Drifter{NewForeman(c, cfg, repo)}
}

var newUnitWorkspaces = execution.NewUnitWorkspaces

var checkpointed = execution.Checkpointed
var captureFiles = execution.CaptureFiles

func (f *Foreman) refresh(ctx context.Context, b execution.Building, rebasing, dispatched map[string]bool, rebased map[string][]string) (bool, error) {
	return f.driver().Refresh(ctx, b, rebasing, dispatched, rebased)
}

func (m *masons) readSealed(w config.WorkstreamID) (execution.Building, bool, error) {
	return m.driver().ReadSealed(w)
}
