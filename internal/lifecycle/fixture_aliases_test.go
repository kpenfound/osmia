package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
)

const InboxDrift = api.InboxDrift

type WorkstreamRebaseResponse = api.WorkstreamRebaseResponse

const TurnDone = api.TurnDone

type ActivityResponse = api.ActivityResponse
type ConfigResponse = api.ConfigResponse

var errWorkgroupJoining = intake.ErrWorkgroupJoining
var sealedPlan = intake.SealedPlan

const driftRejected = intake.DriftRejected

var amendmentMasonTurn = intake.AmendmentMasonTurn

const shedSubject = intake.ShedSubject
const InboxBase = api.InboxBase
const projectSpecCatchingUp = intake.ProjectSpecCatchingUp
const BuildAction = intake.BuildAction

var viewPaths = execution.ViewPaths

type InboxQuestion = api.InboxQuestion

var currentOwnPublication = delivery.CurrentOwnPublication

const sealSubject = intake.SealSubject

type debate = intake.Debate

const mergeDocument = delivery.MergeDocument
const ownPullRequestOutcomeDocument = delivery.OwnPullRequestOutcomeDocument
const BaseDecisionUpstream = api.BaseDecisionUpstream

type apiErrAsError = intake.ApiErrAsError
type DriftStatus = api.DriftStatus
type WorkstreamMoveResponse = api.WorkstreamMoveResponse

const RatifiedState = intake.RatifiedState
const draftInvalid = intake.DraftInvalid

var replyTurnID = intake.ReplyTurnID
var turnKey = execution.TurnKey
var latestRatification = intake.LatestRatification

const ActivityArchiveCleanup = api.ActivityArchiveCleanup
const architectAgent = intake.ArchitectAgent
const NotFound = api.NotFound

var errChangeBoundary = intake.ErrChangeBoundary

const ReplyAction = intake.ReplyAction

var sliceMatchesPin = intake.SliceMatchesPin
var roundTurnID = intake.RoundTurnID

type HandInRequest = api.HandInRequest

const SketchedState = intake.SketchedState

type ProjectAddRequest = api.ProjectAddRequest

var errNoTrace = intake.ErrNoTrace

const architectThread = intake.ArchitectThread
const committeeRole = intake.CommitteeRole

var amendmentSubject = intake.AmendmentSubject

type Identity = api.Identity

const architectRole = intake.ArchitectRole

var deliveryDocuments = delivery.DeliveryDocuments

type FeedEntry = api.FeedEntry
type ownPullRequestOutcome = delivery.OwnPullRequestOutcome

type ProjectRebaseRequest = api.ProjectRebaseRequest

var registrationActor = intake.RegistrationActor

const Unsupported = api.Unsupported

type StatusResponse = api.StatusResponse

const SealAction = intake.SealAction

var nextRevision = intake.NextRevision
var checkSelectionPath = execution.CheckSelectionPath

type RatifyResponse = api.RatifyResponse

var HandInWorkstream = intake.HandInWorkstream

type WorkstreamMoveRequest = api.WorkstreamMoveRequest
type ArchiveResponse = api.ArchiveResponse
type BaseUpstreamResponse = api.BaseUpstreamResponse
type sealer = intake.Sealer
type DriftSkip = api.DriftSkip

const factoryContextTool = intake.FactoryContextTool
const InboxRatification = api.InboxRatification

var unitReviewerSelection = execution.UnitReviewerSelection

type ActivityEntry = api.ActivityEntry
type Diagnostic = api.Diagnostic
type ConversationResponse = api.ConversationResponse

const BackendReady = api.BackendReady
const DraftAction = intake.DraftAction
const InboxDelivery = api.InboxDelivery
const designStartTransition = intake.DesignStartTransition

type ReloadResponse = api.ReloadResponse

const driftResolved = intake.DriftResolved
const handedDocument = intake.HandedDocument

var latestPin = intake.LatestPin

const abandonTransition = intake.AbandonTransition

var chiefOfStaffSkip = intake.ChiefOfStaffSkip

type shedOwner = intake.ShedOwner

const RoundAction = intake.RoundAction
const AbandonedState = intake.AbandonedState
const DraftTool = intake.DraftTool
const InShedState = intake.InShedState

type draftStatus = intake.DraftStatus
type HandInResponse = api.HandInResponse
