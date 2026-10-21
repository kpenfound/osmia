package execution

import (
	"github.com/kpenfound/osmia/internal/bundle"

	"context"

	"github.com/kpenfound/osmia/internal/kb"

	"github.com/kpenfound/osmia/internal/workspace"

	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"

	"github.com/kpenfound/osmia/internal/config"

	"github.com/kpenfound/osmia/internal/trace"

	"github.com/kpenfound/osmia/internal/isolation"

	"github.com/kpenfound/osmia/internal/plan"
)

var MasonTurnID = masonTurnID
var MasonTransitionID = masonTransitionID

func (m *masons) Classify(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (queued, contested bool, err error) {
	return m.classify(ctx, stream, unit, state)
}
func (m *masons) ResumeMasonRuling(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return m.resumeMasonRuling(ctx, stream, unit)
}
func (m *masons) NextStart(streams []Building, entities kb.Map) (int, string, error) {
	return m.nextStart(streams, entities)
}
func (m *masons) RecoverInterrupted(ctx context.Context, stream config.WorkstreamID, unit string) error {
	return m.recoverInterrupted(ctx, stream, unit)
}
func (m *masons) RecoverView(ctx context.Context, stream config.WorkstreamID, agent string, workspace func() (string, error)) error {
	return m.recoverView(ctx, stream, agent, workspace)
}
func (m *masons) RecoverTurn(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return m.recoverTurn(ctx, stream, unit)
}
func (m *masons) Follow(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	return m.follow(ctx, stream, unit, state)
}
func (m *masons) Read(stream config.WorkstreamID) (building, bool, error) { return m.read(stream) }
func (m *masons) ReadActive(stream config.WorkstreamID) (building, bool, error) {
	return m.readActive(stream)
}
func (m *masons) ReadSealed(stream config.WorkstreamID) (building, bool, error) {
	return m.readSealed(stream)
}

var StartOrder = startOrder
var NextReady = nextReady
var DependencyOrder = dependencyOrder

func (m *masons) Bundle(ctx context.Context, stream config.WorkstreamID, unit string) (bundle.Mason, error) {
	return m.bundle(ctx, stream, unit)
}
func (m *masons) Start(ctx context.Context, b Building, unit string) (started, blocked bool, err error) {
	return m.start(ctx, b, unit)
}
func (m *masons) Resume(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return m.resume(ctx, stream, unit)
}

var BlockedSubject = blockedSubject

func (m *masons) Block(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	return m.block(ctx, stream, unit, cause, reason)
}
func (m *masons) Queued(stream config.WorkstreamID, unit string) (bool, error) {
	return m.queued(stream, unit)
}
func (m *masons) Enqueue(ctx context.Context, stream config.WorkstreamID, unit string, mason bundle.Mason, moved trace.Transition) error {
	return m.enqueue(ctx, stream, unit, mason, moved)
}
func (m *masons) EnqueueTurn(ctx context.Context, req trace.TurnRequest) error {
	return m.enqueueTurn(ctx, req)
}

var MasonPrompt = masonPrompt
var Interruption = interruption
var UnitDependenciesMerged = unitDependenciesMerged
var DispatchDocument = dispatchDocument
var DispatchPath = dispatchPath
var StartedDispatch = startedDispatch
var DispatchMessage = dispatchMessage

type DispatchPass = dispatchPass

func (p dispatchPass) Deferral(b Building, unit string) (UnitDispatch, error) {
	return p.deferral(b, unit)
}

var LatestDispatches = latestDispatches

func (m *masons) DispatchRevision(stream config.WorkstreamID, previous trace.Document, d UnitDispatch) (trace.Document, bool, error) {
	return m.dispatchRevision(stream, previous, d)
}
func (m *masons) RecordDeferrals(ctx context.Context, p dispatchPass, streams []Building) error {
	return m.recordDeferrals(ctx, p, streams)
}

var CurrentDeferral = currentDeferral
var UnitBranch = unitBranch
var NewUnitWorkspaces = newUnitWorkspaces
var UnitName = unitName

func (u unitWorkspaces) Open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	return u.open(ctx, stream, unit)
}

const ChangePath = changePath

var ChangeDocument = changeDocument
var ReadUnitChange = unitChange

func (m *masons) ChangeRecord(ctx context.Context, stream config.WorkstreamID, unit string, w workspace.Worktree) (trace.Document, bool, error) {
	return m.changeRecord(ctx, stream, unit, w)
}
func (u unitWorkspaces) Find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	return u.find(ctx, stream, unit)
}
func (u unitWorkspaces) Behind(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return u.behind(ctx, stream, unit)
}
func (u unitWorkspaces) Snapshot(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, string, error) {
	return u.snapshot(ctx, stream, unit)
}
func (u unitWorkspaces) Paths(w workspace.Worktree) ([]string, error) { return u.paths(w) }

var ViewPaths = viewPaths

func (u unitWorkspaces) Selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return u.selection(ctx, scope, execution)
}

const RecordedName = recordedName

var TurnViews = turnViews

func (u unitWorkspaces) Record(ctx context.Context, scope coreadapter.Scope, w workspace.Worktree) error {
	return u.record(ctx, scope, w)
}
func (u unitWorkspaces) Changed(ctx context.Context, stream config.WorkstreamID, unit, turn string) ([]string, bool, error) {
	return u.changed(ctx, stream, unit, turn)
}

var Kept = kept

func (u unitWorkspaces) Capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
	return u.capture(ctx, scope, view, result)
}

type RetriedTurns = retriedTurns

var Mirror = mirror
var Prune = prune
var Copied = copied

const QuestionWaitContestCause = questionWaitContestCause

var LatestUnitTransition = latestUnitTransition
var QuestionWaitOwner = questionWaitOwner
var QuestionWaitCause = questionWaitCause
var QuestionDelivered = questionDelivered
var WaitedQuestion = waitedQuestion

func (m *masons) ContestQuestionWait(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, actor trace.Actor, reason string) error {
	return m.contestQuestionWait(ctx, stream, unit, state, actor, reason)
}

const MoveUnitTool = moveUnitTool

var MoveSources = moveSources
var MoveTargets = moveTargets
var MoveDocument = moveDocument
var MovePath = movePath
var MovePrefix = movePrefix
var IsMove = isMove
var MoveContest = moveContest
var RecordedMoves = recordedMoves
var MoveNote = moveNote
var UnitOperation = unitOperation
var ApprovalRecord = approvalRecord
var MoveResult = moveResult

const ChecksRole = checksRole

var ChecksActor = checksActor

const CheckRetryDelay = checkRetryDelay
const MaxIncompleteChecks = maxIncompleteChecks

type CheckInput = checkInput

func (r UnitCheckRun) Matches(identity UnitReviewIdentity) bool { return r.matches(identity) }

var ReadUnitReport = unitReport
var CheckDocumentID = checkDocumentID
var CheckRequestID = checkRequestID
var CheckPath = checkPath
var CheckPathPattern = checkPathPattern
var ChecksSubject = checksSubject
var ChecksThread = checksThread

func (c *checkers) Scope(in checkInput) coreadapter.Scope { return c.scope(in) }
func (c *checkers) One(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, runs []UnitCheckRun, ops []checkOperation, hold bool) (bool, error) {
	return c.one(ctx, stream, unit, state, runs, ops, hold)
}

var SinceMove = sinceMove

func (c *checkers) Request(ctx context.Context, stream config.WorkstreamID, in checkInput) error {
	return c.request(ctx, stream, in)
}
func (c *checkers) PassUnit(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	return c.pass(ctx, stream, unit, state, run)
}
func (c *checkers) Fail(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run UnitCheckRun) error {
	return c.fail(ctx, stream, unit, state, run)
}

var CheckVerdict = checkVerdict

func (c *checkers) IncompleteSinceEntry(stream config.WorkstreamID, unit string, latest UnitCheckRun, runs []UnitCheckRun) (int, error) {
	return c.incompleteSinceEntry(stream, unit, latest, runs)
}
func (c *checkers) ContestIncomplete(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, latest UnitCheckRun, n int) error {
	return c.contestIncomplete(ctx, stream, unit, state, latest, n)
}
func (c *checkers) Incomplete(ctx context.Context, stream config.WorkstreamID, run UnitCheckRun) error {
	return c.incomplete(ctx, stream, run)
}

var CheckBlockedSubject = checkBlockedSubject

func (c *checkers) Block(ctx context.Context, stream config.WorkstreamID, unit, reason string) error {
	return c.block(ctx, stream, unit, reason)
}
func (c *checkers) Decode(op coreadapter.Operation) (checkInput, error) { return c.decode(op) }
func (c *checkers) Superseded(in checkInput) (string, error)            { return c.superseded(in) }
func (c *checkers) Record(ctx context.Context, in checkInput, run UnitCheckRun, captured string) (coreadapter.OperationResult, error) {
	return c.record(ctx, in, run, captured)
}

var MustJSON = mustJSON
var CheckResult = checkResult
var CheckRuns = checkRuns

type CheckOperation = checkOperation

var CheckOperations = checkOperations
var LatestRunFor = latestRunFor
var CheckEvidence = checkEvidence
var QuoteLinks = quoteLinks
var UnitReviewerIdentity = unitReviewerIdentity
var UnitReviewerSelection = unitReviewerSelection

func (c *checkers) SelectChecks(ctx context.Context, in checkInput, dir, diff string, runner ReviewChecks) CheckSelection {
	return c.selectChecks(ctx, in, dir, diff, runner)
}

const VerdictTool = verdictTool
const VerdictOutcome = verdictOutcome

var ReviewerActor = reviewerActor

type ReviewerReports = reviewerReports

func (r *reviewerReports) Tool(scope coreadapter.Scope) coreadapter.Tool { return r.tool(scope) }

type VerdictTurns = verdictTurns

var ReviewTurnID = reviewTurnID
var AnswerQuestionID = answerQuestionID

func (r *reviewers) FollowQuestion(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	return r.followQuestion(ctx, stream, unit, state)
}
func (r *reviewers) One(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	return r.one(ctx, stream, unit, state, paused)
}
func (r *reviewers) ContestMissingVerdict(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, last trace.QueuedTurn) error {
	return r.contestMissingVerdict(ctx, stream, unit, state, last)
}
func (r *reviewers) ReviewGuidance(stream config.WorkstreamID, unit, candidate string) (string, error) {
	return r.reviewGuidance(stream, unit, candidate)
}
func (r *reviewers) EnsureThread(ctx context.Context, stream config.WorkstreamID, unit string) error {
	return r.ensureThread(ctx, stream, unit)
}
func (r *reviewers) Recover(ctx context.Context, last trace.QueuedTurn) error {
	return r.recover(ctx, last)
}
func (r *reviewers) StoredResult(stream config.WorkstreamID, unit string, state trace.WorkflowState) (UnitReviewResult, bool, error) {
	return r.storedResult(stream, unit, state)
}
func (r *reviewers) TurnIdentity(stream config.WorkstreamID, unit string, turn trace.QueuedTurn) (UnitReviewIdentity, error) {
	return r.turnIdentity(stream, unit, turn)
}
func (r *reviewers) AnswerOrigin(stream config.WorkstreamID, unit string, turn trace.QueuedTurn) (trace.QueuedTurn, error) {
	return r.answerOrigin(stream, unit, turn)
}
func (r *reviewers) FinishReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, turn trace.QueuedTurn) error {
	return r.finishReview(ctx, stream, unit, state, turn)
}

var ValidateVerdict = validateVerdict

func (r *reviewers) ApplyReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult) error {
	return r.applyReview(ctx, stream, unit, state, result)
}

var StaleReview = staleReview

func (m *masons) StaleInputs(ctx context.Context, stream config.WorkstreamID, unit string, reviewed UnitReviewIdentity) (string, error) {
	return m.staleInputs(ctx, stream, unit, reviewed)
}

const MaxReviewRefreshes = maxReviewRefreshes

func (r *reviewers) RefreshReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, reason string) error {
	return r.refreshReview(ctx, stream, unit, state, reason)
}
func (r *reviewers) Recheck(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, identity UnitReviewIdentity) error {
	return r.recheck(ctx, stream, unit, state, identity)
}

var OrdinarySendBack = ordinarySendBack

func (r *reviewers) ResumeFindings(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) error {
	return r.resumeFindings(ctx, stream, unit, state)
}
func (r *reviewers) EnqueueFindings(ctx context.Context, stream config.WorkstreamID, unit string, result UnitReviewResult) error {
	return r.enqueueFindings(ctx, stream, unit, result)
}

var ContestedRulingPath = contestedRulingPath
var MasonRulingPath = masonRulingPath
var MasonContest = masonContest

type MasonRuling = masonRuling

var LatestMasonRuling = latestMasonRuling
var LatestContestedRuling = latestContestedRuling
var RulerName = rulerName
var ContestRuler = contestRuler
var ContestActor = contestActor

func (r *reviewers) ResumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	return r.resumeContested(ctx, stream, unit, state, contests...)
}
func (r *reviewers) ApplyBounceRuling(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult, ruling ContestedRuling, docs []trace.Document) error {
	return r.applyBounceRuling(ctx, stream, unit, state, result, ruling, docs)
}

var BounceRevisionTransition = bounceRevisionTransition

func (m *masons) ResumeBounceRevision(ctx context.Context, stream config.WorkstreamID, unit string, t trace.Transition) error {
	return m.resumeBounceRevision(ctx, stream, unit, t)
}

const LandingTrailer = landingTrailer

var ForemanActor = foremanActor

type LandInput = landInput

var LandingSubject = landingSubject
var LandingDocument = landingDocument
var LandIDs = landIDs

func (f *foreman) Approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	return f.approval(stream, unit, revision)
}
func (f *foreman) Header(id string, stream config.WorkstreamID, unit, cause string, at time.Time) trace.Header {
	return f.header(id, stream, unit, cause, at)
}
func (f *foreman) Request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result UnitReviewResult) error {
	return f.request(ctx, stream, unit, review, result)
}

var DecodeLand = decodeLand

func (f *foreman) Stream(op coreadapter.Operation, in landInput) (config.WorkstreamID, error) {
	return f.stream(op, in)
}
func (f *foreman) Outcome(stream config.WorkstreamID, in landInput) (*coreadapter.OperationResult, error) {
	return f.outcome(stream, in)
}

var LandingCommit = landingCommit

func (f *foreman) Land(ctx context.Context, operation string, stream config.WorkstreamID, in landInput, g workspace.Provider, requested time.Time) (coreadapter.OperationResult, error) {
	return f.land(ctx, operation, stream, in, g, requested)
}
func (f *foreman) RequestedAt(stream config.WorkstreamID, operation string) (time.Time, error) {
	return f.requestedAt(stream, operation)
}
func (f *foreman) Current(ctx context.Context, stream config.WorkstreamID, in landInput, result UnitReviewResult) (string, error) {
	return f.current(ctx, stream, in, result)
}
func (f *foreman) Message(stream config.WorkstreamID, in landInput, review trace.Document, result UnitReviewResult, operation string) (string, error) {
	return f.message(stream, in, review, result, operation)
}

const SubjectLength = subjectLength

var LandingMessage = landingMessage

func (f *foreman) Record(ctx context.Context, stream config.WorkstreamID, in landInput, operation string, review trace.Document, result UnitReviewResult, commit string) (coreadapter.OperationResult, error) {
	return f.record(ctx, stream, in, operation, review, result, commit)
}

var NewlyReady = newlyReady

func (f *foreman) Refuse(ctx context.Context, stream config.WorkstreamID, in landInput, reason string) (coreadapter.OperationResult, error) {
	return f.refuse(ctx, stream, in, reason)
}

const RebaseSnapshotTrailer = rebaseSnapshotTrailer

type RebaseInput = rebaseInput

var RebaseSubject = rebaseSubject
var RebaseDocument = rebaseDocument
var RebaseIDs = rebaseIDs
var ResolveTurnID = resolveTurnID

func (f *foreman) Refresh(ctx context.Context, b Building, rebasing map[string]bool, dispatched map[string]bool, rebased map[string][]string) (bool, error) {
	return f.refresh(ctx, b, rebasing, dispatched, rebased)
}
func (f *foreman) Writing(stream config.WorkstreamID, unit string, dispatched map[string]bool) (bool, error) {
	return f.writing(stream, unit, dispatched)
}
func (f *foreman) RequestRebase(ctx context.Context, stream config.WorkstreamID, unit, onto string) error {
	return f.requestRebase(ctx, stream, unit, onto)
}
func (f *foreman) LandedBy(stream config.WorkstreamID, commit string) (string, error) {
	return f.landedBy(stream, commit)
}
func (m *masons) Conflicts(stream config.WorkstreamID, unit string) ([]string, UnitRebase, error) {
	return m.conflicts(stream, unit)
}
func (f *foreman) Resolve(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (bool, error) {
	return f.resolve(ctx, stream, unit, state)
}
func (m *masons) EnqueueResolve(ctx context.Context, stream config.WorkstreamID, unit string, paths []string, rebase UnitRebase) error {
	return m.enqueueResolve(ctx, stream, unit, paths, rebase)
}

var ResolvePrompt = resolvePrompt
var JujutsuMarkers = jujutsuMarkers

func (m *masons) Remind(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, done trace.QueuedTurn, marked []string) (bool, error) {
	return m.remind(ctx, stream, unit, state, done, marked)
}

var DecodeRebase = decodeRebase

func (r rebaser) Stream(op coreadapter.Operation, in rebaseInput) (config.WorkstreamID, error) {
	return r.stream(op, in)
}
func (r rebaser) Outcome(stream config.WorkstreamID, in rebaseInput) (*coreadapter.OperationResult, error) {
	return r.outcome(stream, in)
}

var RebasedSnapshot = rebasedSnapshot

func (r rebaser) Rebase(ctx context.Context, operation string, stream config.WorkstreamID, in rebaseInput, g workspace.Provider, requested time.Time) (coreadapter.OperationResult, error) {
	return r.rebase(ctx, operation, stream, in, g, requested)
}

var RebaseCarrying = rebaseCarrying
var RebasedConflicts = rebasedConflicts

func (r rebaser) Record(ctx context.Context, stream config.WorkstreamID, in rebaseInput, operation string, rebase UnitRebase) (coreadapter.OperationResult, error) {
	return r.record(ctx, stream, in, operation, rebase)
}
func (r rebaser) Refuse(ctx context.Context, stream config.WorkstreamID, in rebaseInput, reason string) (coreadapter.OperationResult, error) {
	return r.refuse(ctx, stream, in, reason)
}

const DoneTool = doneTool
const MasonDone = masonDone
const ReportPath = reportPath

var ReportDocument = reportDocument
var CheckingTransitionID = checkingTransitionID

type MasonReports = masonReports
type ReportedDone = reportedDone

var TurnKey = turnKey

func (r *masonReports) Tool(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return r.tool(repository, scope)
}
func (r *masonReports) DriftTool(scope coreadapter.Scope) coreadapter.Tool { return r.driftTool(scope) }

var CheckReport = checkReport

type ReportingTurns = reportingTurns

func (m *masons) Reported(stream config.WorkstreamID, unit string) (MasonReport, trace.QueuedTurn, bool, error) {
	return m.reported(stream, unit)
}
func (m *masons) Finish(ctx context.Context, b Building, unit string) (moved, blocked bool, err error) {
	return m.finish(ctx, b, unit)
}

var FinishNotice = finishNotice
var ReviewDocument = reviewDocument

func (m *masons) PrepareUnitReview(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.prepareUnitReview(ctx, stream, unit)
}

var ReviewBlockedSubject = reviewBlockedSubject

func (m *masons) RecordReviewPreparationError(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	return m.recordReviewPreparationError(ctx, stream, unit, cause, reason)
}
func (m *masons) UnitReviewEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.unitReviewEvidence(ctx, stream, unit)
}
func (m *masons) CandidateEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	return m.candidateEvidence(ctx, stream, unit)
}
func (m *masons) Carried(stream config.WorkstreamID, unit string, reviewed, current UnitReviewIdentity) (UnitReviewIdentity, error) {
	return m.carried(stream, unit, reviewed, current)
}

var ReviewDiffTool = reviewDiffTool

const ResolveContestedTool = resolveContestedTool
const ChiefContestLimit = chiefContestLimit
const EscalateContest = escalateContest

var ChiefContestPath = chiefContestPath
var ChiefDecisionDocument = chiefDecisionDocument
var UnitContest = unitContest
var ChiefDecisions = chiefDecisions
var ChiefRulingsLeft = chiefRulingsLeft
var ContestRaised = contestRaised
var RulingResult = rulingResult
var Exhausted = exhausted
var FailureHistory = failureHistory
var FailureContestID = failureContestID
var ContestFailure = contestFailure

const MaxRecoveries = maxRecoveries

var InterruptedInARow = interruptedInARow
var ContestInterrupted = contestInterrupted

func (m *masons) ContestFailure(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (bool, error) {
	return m.contestFailure(ctx, stream, unit, state)
}

var FailedReview = failedReview
var ChecksContest = checksContest
var BounceContest = bounceContest
var CheckSelectionDocument = checkSelectionDocument
var CheckSelectionPath = checkSelectionPath

func (c *checkers) RestoreSelection(in checkInput, run *UnitCheckRun) (bool, error) {
	return c.restoreSelection(in, run)
}
func (c *checkers) Selection(ctx context.Context, in checkInput, run *UnitCheckRun, dir, diff string, checks ReviewChecks) (CheckSelection, error) {
	return c.selection(ctx, in, run, dir, diff, checks)
}

type Building = building

func (b building) Plan() plan.Plan                        { return b.plan }
func (b building) States() map[string]trace.WorkflowState { return b.states }
func (b building) Stream() config.WorkstreamID            { return b.stream }
