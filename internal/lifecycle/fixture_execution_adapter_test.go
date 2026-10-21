package lifecycle_test

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

var unitBranch = execution.UnitBranch

const verdictTool = execution.VerdictTool
const verdictOutcome = execution.VerdictOutcome

var landingDocument = execution.LandingDocument
var rebaseDocument = execution.RebaseDocument
var rebaseIDs = execution.RebaseIDs
var resolveTurnID = execution.ResolveTurnID
var decodeRebase = execution.DecodeRebase

const doneTool = execution.DoneTool
const masonDone = execution.MasonDone

type masonReports = execution.MasonReports
type UnitCheckSelection = execution.UnitCheckSelection

var checkSelectionDocument = execution.CheckSelectionDocument

type building = execution.Building
type masons struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
}

func (m *masons) driver() *execution.Masons {
	var c *execution.Controller
	if m.s != nil {
		c = m.s.execution()
	}
	return execution.NewMasons(c, m.cfg, m.repository)
}

type foreman struct {
	*masons
	nextDrift time.Time
}

func (f *foreman) driver() *execution.Foreman { return execution.NewForeman(f.masons.driver()) }

type reviewers struct{ *masons }

func (r *reviewers) driver() *execution.Reviewers { return execution.NewReviewers(r.masons.driver()) }

type checkers struct{ *masons }

func (c *checkers) driver() *execution.Checkers { return execution.NewCheckers(c.masons.driver()) }

type rebaser struct{ *foreman }

func (r rebaser) driver() execution.Rebaser { return execution.NewRebaser(r.foreman.driver()) }

type unitWorkspaces struct {
	execution.UnitWorkspaces
	streamWorkspaces streamWorkspaces
}

func newUnitWorkspaces(cfg *config.Config, repo *trace.Repository) unitWorkspaces {
	return unitWorkspaces{execution.NewUnitWorkspaces(cfg, repo), streamWorkspaces{Cfg: cfg, Repository: repo, Directory: unitsDirectory}}
}
func (u unitWorkspaces) driver() execution.UnitWorkspaces { return u.UnitWorkspaces }
func (m *masons) Pass(ctx context.Context) error          { return m.driver().Pass(ctx) }
func (m *masons) read(stream config.WorkstreamID) (building, bool, error) {
	return m.driver().Read(stream)
}
func (m *masons) start(ctx context.Context, b building, unit string) (started, blocked bool, err error) {
	return m.driver().Start(ctx, b, unit)
}

func (u unitWorkspaces) open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	return u.driver().Open(ctx, stream, unit)
}
func (u unitWorkspaces) find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	return u.driver().Find(ctx, stream, unit)
}
func (u unitWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return u.driver().Acquire(ctx, req)
}

func (c *checkers) Pass(ctx context.Context) error { return c.driver().Pass(ctx) }

func (c *checkers) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return c.driver().Inspect(ctx, op)
}
func (c *checkers) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return c.driver().Apply(ctx, op)
}

func (r *reviewers) Pass(ctx context.Context) error { return r.driver().Pass(ctx) }
func (r *reviewers) one(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	return r.driver().One(ctx, stream, unit, state, paused)
}
func (f *foreman) Pass(ctx context.Context) error { return f.driver().Pass(ctx) }
func (f *foreman) approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	return f.driver().Approval(stream, unit, revision)
}

func (f *foreman) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return f.driver().Inspect(ctx, op)
}
func (f *foreman) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.driver().Apply(ctx, op)
}
func (f *foreman) current(ctx context.Context, stream config.WorkstreamID, in landInput, result UnitReviewResult) (string, error) {
	return f.driver().Current(ctx, stream, in, result)
}

func (r rebaser) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return r.driver().Inspect(ctx, op)
}
func (r rebaser) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return r.driver().Apply(ctx, op)
}
func (m *masons) finish(ctx context.Context, b building, unit string) (moved, blocked bool, err error) {
	return m.driver().Finish(ctx, b, unit)
}
func (m *masons) prepareUnitReview(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.driver().PrepareUnitReview(ctx, stream, unit)
}

type streamWorkspaces = intake.StreamWorkspaces

const UnitChecking = execution.UnitChecking
