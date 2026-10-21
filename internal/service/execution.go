package service

import (
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/execution"

	"context"

	"github.com/kpenfound/osmia/internal/workspace"

	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"

	"github.com/kpenfound/osmia/internal/config"

	"github.com/kpenfound/osmia/internal/trace"

	"github.com/kpenfound/osmia/internal/isolation"
)

var masonTurnID = execution.MasonTurnID

var interruption = execution.Interruption

const DispatchStarted = execution.DispatchStarted
const DispatchDeferred = execution.DispatchDeferred
const DeferPaused = execution.DeferPaused
const DeferBlocked = execution.DeferBlocked
const DeferEntangled = execution.DeferEntangled
const DeferWorkstreamCap = execution.DeferWorkstreamCap
const DeferPriority = execution.DeferPriority
const DeferCapacity = execution.DeferCapacity

var dispatchDocument = execution.DispatchDocument

var currentDeferral = execution.CurrentDeferral
var unitBranch = execution.UnitBranch

var unitName = execution.UnitName

type UnitChange = execution.UnitChange

var changeDocument = execution.ChangeDocument
var viewPaths = execution.ViewPaths
var turnViews = execution.TurnViews

type retriedTurns = execution.RetriedTurns

var mirror = execution.Mirror

var questionDelivered = execution.QuestionDelivered

const moveUnitTool = execution.MoveUnitTool

var movePrefix = execution.MovePrefix

var moveContest = execution.MoveContest

const CheckAction = execution.CheckAction

const checksRole = execution.ChecksRole

var checksActor = execution.ChecksActor

const checkRetryDelay = execution.CheckRetryDelay
const maxIncompleteChecks = execution.MaxIncompleteChecks

type checkInput = execution.CheckInput
type UnitCheckRun = execution.UnitCheckRun

var checkRuns = execution.CheckRuns

var checkDocumentID = execution.CheckDocumentID

var checkPath = execution.CheckPath
var checkPathPattern = execution.CheckPathPattern
var checksSubject = execution.ChecksSubject

var checkBlockedSubject = execution.CheckBlockedSubject

var quoteLinks = execution.QuoteLinks
var unitReviewerIdentity = execution.UnitReviewerIdentity
var unitReviewerSelection = execution.UnitReviewerSelection

const verdictTool = execution.VerdictTool
const verdictOutcome = execution.VerdictOutcome

var reviewerActor = execution.ReviewerActor

type UnitVerdict = execution.UnitVerdict
type UnitReviewResult = execution.UnitReviewResult
type reviewerReports = execution.ReviewerReports
type verdictTurns = execution.VerdictTurns

var reviewTurnID = execution.ReviewTurnID
var answerQuestionID = execution.AnswerQuestionID

var staleReview = execution.StaleReview

var masonContest = execution.MasonContest

var latestContestedRuling = execution.LatestContestedRuling
var rulerName = execution.RulerName

const LandAction = execution.LandAction
const landingTrailer = execution.LandingTrailer

var foremanActor = execution.ForemanActor

type landInput = execution.LandInput

var landingDocument = execution.LandingDocument
var landIDs = execution.LandIDs
var decodeLand = execution.DecodeLand

const subjectLength = execution.SubjectLength

const RebaseAction = execution.RebaseAction

var rebaseDocument = execution.RebaseDocument
var rebaseIDs = execution.RebaseIDs
var resolveTurnID = execution.ResolveTurnID

var jujutsuMarkers = execution.JujutsuMarkers
var decodeRebase = execution.DecodeRebase

const doneTool = execution.DoneTool
const masonDone = execution.MasonDone
const reportPath = execution.ReportPath

type MasonReport = execution.MasonReport
type UnitReport = execution.UnitReport

var reportDocument = execution.ReportDocument

type masonReports = execution.MasonReports

var turnKey = execution.TurnKey

type reportingTurns = execution.ReportingTurns

var reviewDocument = execution.ReviewDocument

var reviewDiffTool = execution.ReviewDiffTool

const resolveContestedTool = execution.ResolveContestedTool

const escalateContest = execution.EscalateContest

var chiefContestPath = execution.ChiefContestPath

var unitContest = execution.UnitContest
var chiefDecisions = execution.ChiefDecisions
var chiefRulingsLeft = execution.ChiefRulingsLeft
var contestRaised = execution.ContestRaised

var exhausted = execution.Exhausted
var failureHistory = execution.FailureHistory

const maxRecoveries = execution.MaxRecoveries

var interruptedInARow = execution.InterruptedInARow

var failedReview = execution.FailedReview
var checksContest = execution.ChecksContest

type UnitCheckSelection = execution.UnitCheckSelection

var checkSelectionDocument = execution.CheckSelectionDocument
var checkSelectionPath = execution.CheckSelectionPath

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

func (m *masons) recoverView(ctx context.Context, stream config.WorkstreamID, agent string, workspace func() (string, error)) error {
	return m.driver().RecoverView(ctx, stream, agent, workspace)
}

func (m *masons) read(stream config.WorkstreamID) (building, bool, error) {
	return m.driver().Read(stream)
}

func (m *masons) readSealed(stream config.WorkstreamID) (building, bool, error) {
	return m.driver().ReadSealed(stream)
}
func (m *masons) bundle(ctx context.Context, stream config.WorkstreamID, unit string) (bundle.Mason, error) {
	return m.driver().Bundle(ctx, stream, unit)
}
func (m *masons) start(ctx context.Context, b building, unit string) (started, blocked bool, err error) {
	return m.driver().Start(ctx, b, unit)
}

func (m *masons) block(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	return m.driver().Block(ctx, stream, unit, cause, reason)
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
func (u unitWorkspaces) snapshot(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, string, error) {
	return u.driver().Snapshot(ctx, stream, unit)
}
func (u unitWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return u.driver().Acquire(ctx, req)
}

func (u unitWorkspaces) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return u.driver().Selection(ctx, scope, execution)
}

func (u unitWorkspaces) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
	return u.driver().Capture(ctx, scope, view, result)
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

func (r *reviewers) storedResult(stream config.WorkstreamID, unit string, state trace.WorkflowState) (UnitReviewResult, bool, error) {
	return r.driver().StoredResult(stream, unit, state)
}

func (r *reviewers) refreshReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, reason string) error {
	return r.driver().RefreshReview(ctx, stream, unit, state, reason)
}

func (f *foreman) Pass(ctx context.Context) error { return f.driver().Pass(ctx) }
func (f *foreman) approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	return f.driver().Approval(stream, unit, revision)
}
func (f *foreman) header(id string, stream config.WorkstreamID, unit, cause string, at time.Time) trace.Header {
	return f.driver().Header(id, stream, unit, cause, at)
}

func (f *foreman) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return f.driver().Inspect(ctx, op)
}
func (f *foreman) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.driver().Apply(ctx, op)
}

func (f *foreman) requestedAt(stream config.WorkstreamID, operation string) (time.Time, error) {
	return f.driver().RequestedAt(stream, operation)
}
func (f *foreman) current(ctx context.Context, stream config.WorkstreamID, in landInput, result UnitReviewResult) (string, error) {
	return f.driver().Current(ctx, stream, in, result)
}

func (f *foreman) refresh(ctx context.Context, b building, rebasing map[string]bool, dispatched map[string]bool, rebased map[string][]string) (bool, error) {
	return f.driver().Refresh(ctx, b, rebasing, dispatched, rebased)
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

func (c *checkers) incompleteSinceEntry(stream config.WorkstreamID, unit string, latest UnitCheckRun, runs []UnitCheckRun) (int, error) {
	return c.driver().IncompleteSinceEntry(stream, unit, latest, runs)
}

func (f *foreman) request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result UnitReviewResult) error {
	return f.driver().Request(ctx, stream, unit, review, result)
}
