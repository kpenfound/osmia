package intakejourney

import (
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const AbandonedState = intake.AbandonedState
const DeliveredState = intake.DeliveredState
const abandonTransition = intake.AbandonTransition
const cancelReason = intake.CancelReason

var abandonActor = intake.AbandonActor

const amendmentApplied = intake.AmendmentApplied

var amendmentUnitID = intake.AmendmentUnitID
var amendmentMasonTurn = intake.AmendmentMasonTurn

const AmendmentRoundAction = intake.AmendmentRoundAction
const AmendmentReplyAction = intake.AmendmentReplyAction

type amendmentDebate = intakefixture.AmendmentDebate

var amendmentReplyPath = intake.AmendmentReplyPath
var amendmentPacketPath = intake.AmendmentPacketPath

var amendmentRecords = intake.AmendmentRecords

const AmendmentApprove = intake.AmendmentApprove
const AmendmentReject = intake.AmendmentReject
const AmendmentRound = intake.AmendmentRound
const AmendmentOverrule = intake.AmendmentOverrule
const amendmentPresented = intake.AmendmentPresented
const amendmentApproved = intake.AmendmentApproved
const amendmentRejected = intake.AmendmentRejected
const amendmentResealed = intake.AmendmentResealed
const amendmentUnapplied = intake.AmendmentUnapplied
const amendmentRuled = intake.AmendmentRuled

var amendmentActor = intake.AmendmentActor

var amendmentDecisionID = intake.AmendmentDecisionID
var amendmentDecided = intake.AmendmentDecided

const AmendmentDraftAction = intake.AmendmentDraftAction
const amendmentWaiting = intake.AmendmentWaiting

type amendmentDrafter = intake.AmendmentDrafter
type amendmentDraftInput = intake.AmendmentDraftInput

var amendmentSubject = intakefixture.AmendmentSubject

type amendmentAffected = intake.AmendmentAffected

var affectedRevision = intake.AffectedRevision

const DraftAction = intake.DraftAction
const architectRole = intakefixture.ArchitectRole

const architectAgent = intake.ArchitectAgent
const architectThread = intake.ArchitectThread
const SketchedState = intakefixture.SketchedState

const DraftTool = intakefixture.DraftTool

const draftSubject = intake.DraftSubject
const maxDrafts = intake.MaxDrafts
const maxDraftAttempts = intake.MaxDraftAttempts

var architectActor = intake.ArchitectActor
var draftingActor = intake.DraftingActor

type Architect = intake.Architect
type draftInput = intake.DraftInput

var draftTurnID = intake.DraftTurnID

type drafter = intake.Drafter

var errNoArchitect = intake.ErrNoArchitect

const draftValid = intakefixture.DraftValid

const draftInvalid = intake.DraftInvalid
const draftDeclined = intake.DraftDeclined
const draftUnchanged = intake.DraftUnchanged

type draftStatus = intakefixture.DraftStatus

var draftNext = intake.DraftNext

var chainTurns = intake.ChainTurns

const BuildAction = intake.BuildAction
const BuildingState = intakefixture.BuildingState

const buildSubject = intakefixture.BuildSubject

const UnitPlanned = intake.UnitPlanned
const UnitReady = intake.UnitReady
const UnitImplementing = intakefixture.UnitImplementing

const UnitReviewing = intake.UnitReviewing
const UnitApproved = intake.UnitApproved
const UnitMerged = intake.UnitMerged
const UnitWaiting = intakefixture.UnitWaiting

const UnitContested = intake.UnitContested

type buildInput = intake.BuildInput

var buildIDs = intake.BuildIDs

type builder = intake.Builder

var errNoActiveProject = intake.ErrNoActiveProject

type charterer = intake.Charterer

var committeePerspectives = intake.CommitteePerspectives

var chiefActor = intakefixture.ChiefActor

var ownerActor = intake.OwnerActor

const draftBriefToolName = intakefixture.DraftBriefToolName

const designReadRepoToolName = intake.DesignReadRepoToolName
const designDraftDocument = intakefixture.DesignDraftDocument

const designBriefPath = intake.DesignBriefPath
const designTranscriptHeading = intake.DesignTranscriptHeading

type designBriefRecord = intakefixture.DesignBriefRecord

var isDesignWorkstream = intake.IsDesignWorkstream

const affectedReposTool = intake.AffectedReposTool
const designStartSubject = intake.DesignStartSubject

var designSelectionOf = intake.DesignSelectionOf

var librarianWorkstream = intake.LibrarianWorkstream

type stagedWorkspaces = intakefixture.StagedWorkspaces

const factoryPolicySubject = intake.FactoryPolicySubject

const AssembledState = intake.AssembledState

const MaxHandedBytes = intake.MaxHandedBytes
const handedDocument = intake.HandedDocument
const handInTransition = intake.HandInTransition
const handInSkipReason = intake.HandInSkipReason
const HandedState = intake.HandedState

var HandInWorkstream = intake.HandInWorkstream

var inboxView = intake.InboxView

var featureWorkspaces = intake.FeatureWorkspaces

var masonActor = intake.MasonActor
var masonAgent = intakefixture.MasonAgent

type objectionDecisionRequest = intake.ObjectionDecisionRequest

var serviceActor = intake.ServiceActor

const questionAssessmentTask = intake.QuestionAssessmentTask
const questionAssessmentVersion = intake.QuestionAssessmentVersion

const amendmentQuestion = intake.AmendmentQuestion
const conflictQuestion = intake.ConflictQuestion
const scopeQuestion = intake.ScopeQuestion
const noRuling = intake.NoRuling
const questionAdvice = intake.QuestionAdvice

const reviewerRole = intakefixture.ReviewerRole

var reviewerAgent = intake.ReviewerAgent

const SealAction = intake.SealAction
const RatifiedState = intake.RatifiedState
const sealSubject = intake.SealSubject
const branchesDirectory = intake.BranchesDirectory

var sealingActor = intake.SealingActor

type sealInput = intake.SealInput
type ratification = intake.Ratification

var featureBranch = intake.FeatureBranch

type sealer = intake.Sealer

var latestRatification = intake.LatestRatification
var decodeSeal = intake.DecodeSeal

const RoundAction = intake.RoundAction
const committeeRole = intakefixture.CommitteeRole

const InShedState = intake.InShedState
const shedSubject = intake.ShedSubject
const ownerSubject = intake.OwnerSubject
const skippedValue = intake.SkippedValue
const skipTransition = intake.SkipTransition
const maxRoundAttempts = intake.MaxRoundAttempts

var shedActor = intake.ShedActor

type Committee = intake.Committee
type roundInput = intake.RoundInput

var committeeAgent = intakefixture.CommitteeAgent

var committeeThread = intake.CommitteeThread

var roundTurnID = intake.RoundTurnID

type debate = intake.Debate

var presentation = intake.Presentation
var decideFirst = intake.DecideFirst
var unopposed = intake.Unopposed
var Dissent = intake.Dissent

var decodeRound = intake.DecodeRound
var decodeShed = intake.DecodeShed

var errNoCommittee = intake.ErrNoCommittee

var ownerHeader = intake.OwnerHeader

var skippedDebate = intake.SkippedDebate
var featureState = intake.FeatureState

const ReplyAction = intake.ReplyAction
const RedraftAction = intake.RedraftAction
const maxRedrafts = intake.MaxRedrafts
const maxReplyAttempts = intake.MaxReplyAttempts
const replyFile = intake.ReplyFile

var replyTurnID = intake.ReplyTurnID

type replier = intake.Replier

const masonRole = intakefixture.MasonRole

const unitsDirectory = intake.UnitsDirectory

var workspaces = intake.Workspaces
