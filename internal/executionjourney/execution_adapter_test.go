package executionjourney

import (
	"time"

	"github.com/kpenfound/osmia/internal/execution"

	"context"

	"github.com/kpenfound/osmia/internal/intake"

	"github.com/kpenfound/osmia/internal/workspace"

	"github.com/kpenfound/osmia/internal/coreadapter"

	"github.com/kpenfound/osmia/internal/config"

	"github.com/kpenfound/osmia/internal/trace"

	"github.com/kpenfound/osmia/internal/isolation"
)

var masonTransitionID = execution.MasonTransitionID

var blockedSubject = execution.BlockedSubject

const DispatchDeferred = execution.DispatchDeferred
const DeferPaused = execution.DeferPaused
const DeferBlocked = execution.DeferBlocked
const DeferEntangled = execution.DeferEntangled
const DeferWorkstreamCap = execution.DeferWorkstreamCap
const DeferPriority = execution.DeferPriority
const DeferCapacity = execution.DeferCapacity

var unitBranch = execution.UnitBranch

var unitName = execution.UnitName

type UnitChange = execution.UnitChange

var changeDocument = execution.ChangeDocument
var unitChange = execution.ReadUnitChange

var turnViews = execution.TurnViews

var isMove = execution.IsMove

const checkRetryDelay = execution.CheckRetryDelay
const maxIncompleteChecks = execution.MaxIncompleteChecks

type checkInput = execution.CheckInput
type UnitCheckRun = execution.UnitCheckRun

var checkDocumentID = execution.CheckDocumentID

var checksSubject = execution.ChecksSubject

var checkRuns = execution.CheckRuns

var latestRunFor = execution.LatestRunFor

const verdictTool = execution.VerdictTool
const verdictOutcome = execution.VerdictOutcome

var staleReview = execution.StaleReview

const maxReviewRefreshes = execution.MaxReviewRefreshes

var contestedRulingPath = execution.ContestedRulingPath

var latestMasonRuling = execution.LatestMasonRuling
var latestContestedRuling = execution.LatestContestedRuling

var contestRuler = execution.ContestRuler

const landingTrailer = execution.LandingTrailer

var landingDocument = execution.LandingDocument

type rebaseInput = execution.RebaseInput

var rebaseSubject = execution.RebaseSubject
var rebaseDocument = execution.RebaseDocument
var rebaseIDs = execution.RebaseIDs
var resolveTurnID = execution.ResolveTurnID

var decodeRebase = execution.DecodeRebase

const doneTool = execution.DoneTool
const masonDone = execution.MasonDone
const reportPath = execution.ReportPath

type UnitReport = execution.UnitReport

var reportDocument = execution.ReportDocument
var checkingTransitionID = execution.CheckingTransitionID

type masonReports = execution.MasonReports

var reviewBlockedSubject = execution.ReviewBlockedSubject

const resolveContestedTool = execution.ResolveContestedTool
const chiefContestLimit = execution.ChiefContestLimit

var chiefDecisionDocument = execution.ChiefDecisionDocument
var chiefDecisions = execution.ChiefDecisions
var chiefRulingsLeft = execution.ChiefRulingsLeft
var contestRaised = execution.ContestRaised

const maxRecoveries = execution.MaxRecoveries

var failedReview = execution.FailedReview
var checksContest = execution.ChecksContest
var bounceContest = execution.BounceContest

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

func (m *masons) follow(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	return m.driver().Follow(ctx, stream, unit, state)
}
func (m *masons) read(stream config.WorkstreamID) (building, bool, error) {
	return m.driver().Read(stream)
}

func (m *masons) start(ctx context.Context, b building, unit string) (started, blocked bool, err error) {
	return m.driver().Start(ctx, b, unit)
}
func (m *masons) resume(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return m.driver().Resume(ctx, stream, unit)
}

func (u unitWorkspaces) open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	return u.driver().Open(ctx, stream, unit)
}

func (u unitWorkspaces) find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	return u.driver().Find(ctx, stream, unit)
}
func (u unitWorkspaces) behind(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return u.driver().Behind(ctx, stream, unit)
}

func (u unitWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return u.driver().Acquire(ctx, req)
}
func (u unitWorkspaces) paths(w workspace.Worktree) ([]string, error) { return u.driver().Paths(w) }
func (u unitWorkspaces) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return u.driver().Selection(ctx, scope, execution)
}

func (c *checkers) Pass(ctx context.Context) error { return c.driver().Pass(ctx) }

func (c *checkers) fail(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	return c.driver().Fail(ctx, stream, unit, state, run)
}

func (c *checkers) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return c.driver().Inspect(ctx, op)
}
func (c *checkers) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return c.driver().Apply(ctx, op)
}

func (c *checkers) record(ctx context.Context, in checkInput, run UnitCheckRun, captured string) (coreadapter.OperationResult, error) {
	return c.driver().Record(ctx, in, run, captured)
}

func (r *reviewers) Pass(ctx context.Context) error { return r.driver().Pass(ctx) }
func (r *reviewers) followQuestion(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	return r.driver().FollowQuestion(ctx, stream, unit, state)
}
func (r *reviewers) one(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	return r.driver().One(ctx, stream, unit, state, paused)
}

func (r *reviewers) storedResult(stream config.WorkstreamID, unit string, state trace.WorkflowState) (UnitReviewResult, bool, error) {
	return r.driver().StoredResult(stream, unit, state)
}

func (r *reviewers) applyReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult) error {
	return r.driver().ApplyReview(ctx, stream, unit, state, result)
}

func (r *reviewers) refreshReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, reason string) error {
	return r.driver().RefreshReview(ctx, stream, unit, state, reason)
}

func (r *reviewers) resumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	return r.driver().ResumeContested(ctx, stream, unit, state, contests...)
}
func (r *reviewers) applyBounceRuling(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult, ruling ContestedRuling, docs []trace.Document) error {
	return r.driver().ApplyBounceRuling(ctx, stream, unit, state, result, ruling, docs)
}
func (m *masons) resumeBounceRevision(ctx context.Context, stream config.WorkstreamID, unit string, t trace.Transition) error {
	return m.driver().ResumeBounceRevision(ctx, stream, unit, t)
}
func (f *foreman) Pass(ctx context.Context) error { return f.driver().Pass(ctx) }
func (f *foreman) approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	return f.driver().Approval(stream, unit, revision)
}

func (f *foreman) request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result UnitReviewResult) error {
	return f.driver().Request(ctx, stream, unit, review, result)
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

func (f *foreman) writing(stream config.WorkstreamID, unit string, dispatched map[string]bool) (bool, error) {
	return f.driver().Writing(stream, unit, dispatched)
}
func (f *foreman) requestRebase(ctx context.Context, stream config.WorkstreamID, unit, onto string) error {
	return f.driver().RequestRebase(ctx, stream, unit, onto)
}

func (m *masons) conflicts(stream config.WorkstreamID, unit string) ([]string, UnitRebase, error) {
	return m.driver().Conflicts(stream, unit)
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

func (m *masons) candidateEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.driver().CandidateEvidence(ctx, stream, unit)
}

type streamWorkspaces = intake.StreamWorkspaces

const UnitChecking = execution.UnitChecking

func (f *foreman) message(w config.WorkstreamID, in landInput, review trace.Document, result UnitReviewResult, op string) (string, error) {
	return f.driver().Message(w, in, review, result, op)
}
func (f *foreman) requestedAt(w config.WorkstreamID, op string) (time.Time, error) {
	return f.driver().RequestedAt(w, op)
}
