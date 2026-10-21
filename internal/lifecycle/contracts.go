package lifecycle

import (
	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
)

const ownPullRequestOutcomeClosed = delivery.OwnPullRequestOutcomeClosed
const Internal = api.Internal

type ownPullRequestOutcome = delivery.OwnPullRequestOutcome

var ownPullRequestFinished = delivery.OwnPullRequestFinished

type FinalRebase = delivery.FinalRebase

var exhausted = execution.Exhausted
var handInKey = intake.HandInKey
var featureState = intake.FeatureState
var ownPullRequestOutcomeDocumentFor = delivery.OwnPullRequestOutcomeDocumentFor

const UnitImplementing = intake.UnitImplementing
const maxRecoveries = execution.MaxRecoveries

var queuedTurn = intake.QueuedTurn

type ActivityHandle = activity.Handle
type DriftCoverage = api.DriftCoverage

const reviewerRole = intake.ReviewerRole
const verdictOutcome = execution.VerdictOutcome

var failureHistory = execution.FailureHistory
var featureWorkspaces = intake.FeatureWorkspaces

type UnitRebase = api.UnitRebase

var serviceActor = intake.ServiceActor

const committeeRole = intake.CommitteeRole

type DeliveryMerge = api.DeliveryMerge
type ProjectRebaseRequest = api.ProjectRebaseRequest

const DeliveredState = intake.DeliveredState

type amendmentDebate = intake.AmendmentDebate

const draftInvalid = intake.DraftInvalid

var committeeAgent = intake.CommitteeAgent

type draftStatus = intake.DraftStatus

const Conflict = api.Conflict
const Validation = api.Validation

var librarianWorkstream = intake.LibrarianWorkstream

type ArchiveResponse = api.ArchiveResponse

var latestFinalReport = delivery.LatestFinalReport

const ownPullRequestOutcomeMerged = delivery.OwnPullRequestOutcomeMerged

type streamWorkspaces = intake.StreamWorkspaces

const RefreshAction = "kb-refresh"
const handedDocument = intake.HandedDocument
const driftsDirectory = intake.DriftsDirectory

type noLease = intake.NoLease

var viewPaths = execution.ViewPaths

const acceptanceDraftInstructions = intake.AcceptanceDraftInstructions

var latestRatification = intake.LatestRatification

const FinalReviewAction = delivery.FinalReviewAction
const UnitWaiting = intake.UnitWaiting
const UnitMerged = intake.UnitMerged

var featureBranch = intake.FeatureBranch

const mergeDocument = delivery.MergeDocument
const finalRebaseDocument = delivery.FinalRebaseDocument

type ShedRedraftRequest = api.ShedRedraftRequest

var sealedPlan = intake.SealedPlan

const RebaseAction = execution.RebaseAction

type WorkstreamRebaseResponse = api.WorkstreamRebaseResponse

const ActivityArchiveCleanup = api.ActivityArchiveCleanup

var reportDocument = execution.ReportDocument
var ownerActor = intake.OwnerActor

type debate = intake.Debate
type UnitVerdict = execution.UnitVerdict

const LandAction = execution.LandAction

var decodeRebase = execution.DecodeRebase
var currentOwnPublication = delivery.CurrentOwnPublication

const BuildingState = intake.BuildingState

var foremanActor = execution.ForemanActor

const driftResolved = intake.DriftResolved

type UnitReport = execution.UnitReport

const architectAgent = intake.ArchitectAgent
const unitsDirectory = intake.UnitsDirectory
const RatifiedState = intake.RatifiedState
const AbandonedState = intake.AbandonedState
const driftRejected = intake.DriftRejected
const SealAction = intake.SealAction

type ShedMoreRequest = api.ShedMoreRequest

var chiefActor = intake.ChiefActor

const publicationOpened = delivery.PublicationOpened

var deliveryMerge = delivery.ReadDeliveryMerge

type WorkstreamMoveResponse = api.WorkstreamMoveResponse
type BaseUpstreamRequest = api.BaseUpstreamRequest

var recordOwnPullRequestOutcomeFromFind = delivery.RecordOwnPullRequestOutcomeFromFind
var nextRevision = intake.NextRevision

const driftReviewerAgent = intake.DriftReviewerAgent

type APIError = api.APIError

const ownPullRequestOutcomeDocument = delivery.OwnPullRequestOutcomeDocument

type ProjectRebaseResponse = api.ProjectRebaseResponse

var abandonActor = intake.AbandonActor

const UnitReady = intake.UnitReady

var HandInWorkstream = intake.HandInWorkstream

const masonRole = intake.MasonRole

var interruption = execution.Interruption

const architectRole = intake.ArchitectRole

type drafter = intake.Drafter

const SketchedState = intake.SketchedState
const branchesDirectory = intake.BranchesDirectory
const draftValid = intake.DraftValid

var rulerName = execution.RulerName
var publications = delivery.Publications

const driftMasonAgent = intake.DriftMasonAgent
const InShedState = intake.InShedState

type WorkstreamMoveRequest = api.WorkstreamMoveRequest

const PublishAction = delivery.PublishAction

var ownPullRequestOutcomeAt = delivery.OwnPullRequestOutcomeAt
var latestPin = intake.LatestPin

type stagedWorkspaces = intake.StagedWorkspaces

var errNoPullRequestCredential = delivery.ErrNoPullRequestCredential
var isSession = intake.IsSession

const verdictTool = execution.VerdictTool
const NotFound = api.NotFound

type RatifyRequest = api.RatifyRequest
type sealer = intake.Sealer

var priorityRefusal = intake.PriorityRefusal

type DeliveryPublication = api.DeliveryPublication

var reviewerAgent = intake.ReviewerAgent

type WorkstreamMove = api.WorkstreamMove

var allMerged = delivery.AllMerged

type DriftSkip = api.DriftSkip

var rebaseDocument = execution.RebaseDocument
var answerQuestionID = execution.AnswerQuestionID

const architectThread = intake.ArchitectThread

var draftNext = intake.DraftNext
var abandoned = intake.Abandoned

type AmendmentDecision = api.AmendmentDecision

var errWorkgroupJoining = intake.ErrWorkgroupJoining
var jujutsuMarkers = execution.JujutsuMarkers
var finalRoundsWithBlocking = delivery.FinalRoundsWithBlocking

const maxFinalRounds = intake.MaxFinalRounds

var finalReportBaseStale = delivery.FinalReportBaseStale

const UnitPlanned = intake.UnitPlanned

type AbandonRequest = api.AbandonRequest

var unitName = execution.UnitName
var pullRequestOutcomeOf = delivery.PullRequestOutcomeOf

type BaseUpstreamResponse = api.BaseUpstreamResponse

const finalReviewed = delivery.FinalReviewed

type DriftStatus = api.DriftStatus

var amendmentActor = intake.AmendmentActor

const AssembledState = intake.AssembledState

var interruptedInARow = execution.InterruptedInARow

const publicationDocument = delivery.PublicationDocument
const designStartSubject = intake.DesignStartSubject

type ActivityKind = api.ActivityKind
type AbandonResponse = api.AbandonResponse
type WorkspacesStatus = api.WorkspacesStatus
type RatifyResponse = api.RatifyResponse
type ShedResponse = api.ShedResponse

type StreamWorkspaces = intake.StreamWorkspaces
