package execution

import (
	"context"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"time"
)

func (m *masons) Read(stream config.WorkstreamID) (building, bool, error) { return m.read(stream) }

func (m *masons) Start(ctx context.Context, b Building, unit string) (started, blocked bool, err error) {
	return m.start(ctx, b, unit)
}

func (m *masons) Block(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	return m.block(ctx, stream, unit, cause, reason)
}

func (u unitWorkspaces) Open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	return u.open(ctx, stream, unit)
}

func (u unitWorkspaces) Selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return u.selection(ctx, scope, execution)
}

func (u unitWorkspaces) Record(ctx context.Context, scope coreadapter.Scope, w workspace.Worktree) error {
	return u.record(ctx, scope, w)
}

func (u unitWorkspaces) Capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
	return u.capture(ctx, scope, view, result)
}

type RetriedTurns = retriedTurns

var Prune = prune

type CheckInput = checkInput

func (c *checkers) One(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, runs []UnitCheckRun, ops []checkOperation, hold bool) (bool, error) {
	return c.one(ctx, stream, unit, state, runs, ops, hold)
}

func (c *checkers) Request(ctx context.Context, stream config.WorkstreamID, in checkInput) error {
	return c.request(ctx, stream, in)
}

func (c *checkers) Block(ctx context.Context, stream config.WorkstreamID, unit, reason string) error {
	return c.block(ctx, stream, unit, reason)
}
func (c *checkers) Record(ctx context.Context, in checkInput, run UnitCheckRun, captured string) (coreadapter.OperationResult, error) {
	return c.record(ctx, in, run, captured)
}

var CheckResult = checkResult

var CheckEvidence = checkEvidence

type ReviewerReports = reviewerReports

func (r *reviewerReports) Tool(scope coreadapter.Scope) coreadapter.Tool { return r.tool(scope) }

type VerdictTurns = verdictTurns

func (r *reviewers) One(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	return r.one(ctx, stream, unit, state, paused)
}

func (r *reviewers) ResumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	return r.resumeContested(ctx, stream, unit, state, contests...)
}
func (r *reviewers) ApplyBounceRuling(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult, ruling ContestedRuling, docs []trace.Document) error {
	return r.applyBounceRuling(ctx, false, stream, unit, state, result, ruling, docs)
}

var ForemanActor = foremanActor

func (f *foreman) Approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	return f.approval(stream, unit, revision)
}
func (f *foreman) Request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result UnitReviewResult) error {
	return f.request(ctx, stream, unit, review, result, 1)
}

func (f *foreman) Stream(op coreadapter.Operation, in LandInput) (config.WorkstreamID, error) {
	return f.stream(op, in)
}
func (f *foreman) Outcome(stream config.WorkstreamID, in LandInput) (*coreadapter.OperationResult, error) {
	return f.outcome(stream, in)
}

func (f *foreman) Land(ctx context.Context, operation string, stream config.WorkstreamID, in LandInput, g workspace.Provider, requested time.Time) (coreadapter.OperationResult, error) {
	return f.land(ctx, operation, stream, in, g, requested, recording{})
}

func (f *foreman) Record(ctx context.Context, stream config.WorkstreamID, in LandInput, operation string, review trace.Document, result UnitReviewResult, commit string) (coreadapter.OperationResult, error) {
	return f.record(ctx, stream, in, operation, review, result, commit, "")
}

func (f *foreman) Refuse(ctx context.Context, stream config.WorkstreamID, in LandInput, reason string) (coreadapter.OperationResult, error) {
	return f.refuse(ctx, stream, in, reason)
}

func (r rebaser) Stream(op coreadapter.Operation, in RebaseInput) (config.WorkstreamID, error) {
	return r.stream(op, in)
}
func (r rebaser) Outcome(stream config.WorkstreamID, in RebaseInput) (*coreadapter.OperationResult, error) {
	return r.outcome(stream, in)
}

func (r rebaser) Rebase(ctx context.Context, operation string, stream config.WorkstreamID, in RebaseInput, g workspace.Provider, requested time.Time) (coreadapter.OperationResult, error) {
	return r.rebase(ctx, operation, stream, in, g, requested, recording{})
}

func (r rebaser) Record(ctx context.Context, stream config.WorkstreamID, in RebaseInput, operation string, rebase UnitRebase) (coreadapter.OperationResult, error) {
	return r.record(ctx, stream, in, operation, rebase, "")
}
func (r rebaser) Refuse(ctx context.Context, stream config.WorkstreamID, in RebaseInput, reason string) (coreadapter.OperationResult, error) {
	return r.refuse(ctx, stream, in, reason)
}

type MasonReports = masonReports

func (r *masonReports) Tool(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return r.tool(repository, scope)
}

type ReportingTurns = reportingTurns

func (m *masons) Finish(ctx context.Context, b Building, unit string) (moved, blocked bool, err error) {
	return m.finish(ctx, b, unit)
}

func (m *masons) PrepareUnitReview(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.prepareUnitReview(ctx, stream, unit)
}

func (c *checkers) Selection(ctx context.Context, in checkInput, run *UnitCheckRun, dir, diff string, checks ReviewChecks) (CheckSelection, error) {
	return c.selection(ctx, in, run, dir, diff, checks)
}

type Building = building

func (b building) Plan() plan.Plan                        { return b.plan }
func (b building) States() map[string]trace.WorkflowState { return b.states }
func (b building) Stream() config.WorkstreamID            { return b.stream }
