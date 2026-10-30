package service

import "github.com/kpenfound/osmia/internal/api"

type ActivityKind = api.ActivityKind

const ActivityArchiveCleanup = api.ActivityArchiveCleanup

const ActivityGit = api.ActivityGit

const ActivityCheck = api.ActivityCheck

const ActivityAgentTurn = api.ActivityAgentTurn

const ActivityRecovery = api.ActivityRecovery

type ActivityEntry = api.ActivityEntry

type ActivityResponse = api.ActivityResponse

type AmendmentDecision = api.AmendmentDecision

type AmendmentDecisionRequest = api.AmendmentDecisionRequest

type AmendmentResponse = api.AmendmentResponse

type UnitStatus = api.UnitStatus

type CharterDecisionRequest = api.CharterDecisionRequest

type CharterProposalView = api.CharterProposalView

type CharterProposalsResponse = api.CharterProposalsResponse

type UnitActivity = api.UnitActivity

type ActivityContest = api.ActivityContest

type ActivityTransition = api.ActivityTransition

type ActivityTurn = api.ActivityTurn

type ActivityRefusal = api.ActivityRefusal

type ChiefContestDecision = api.ChiefContestDecision

type DeliveryPresentation = api.DeliveryPresentation

type DeliveryApproval = api.DeliveryApproval

type DeliveryDecision = api.DeliveryDecision

type UnitDispatch = api.UnitDispatch

type DraftEdit = api.DraftEdit

type CharterEdit = api.CharterEdit

type BaseEdit = api.BaseEdit

type FinalReport = api.FinalReport

type FinalCriterion = api.FinalCriterion

type FinalFinding = api.FinalFinding

type UnitLanding = api.UnitLanding

type MemorySetup = api.MemorySetup

type MemoryAnchor = api.MemoryAnchor

type DeliveryMerge = api.DeliveryMerge

type OverlapAdvisory = api.OverlapAdvisory

type DeliveryPublication = api.DeliveryPublication

type UnitRebase = api.UnitRebase

type CheckResult = api.CheckResult

type ContestedRuling = api.ContestedRuling

type ContestedRulingRequest = api.ContestedRulingRequest

type ContestedRulingResponse = api.ContestedRulingResponse

type ReviewFinding = api.ReviewFinding

type CheckSelection = api.CheckSelection

type TraceSummary = api.TraceSummary

type TraceGap = api.TraceGap

type TraceRef = api.TraceRef

type TraceSeal = api.TraceSeal

type TraceCriterion = api.TraceCriterion

type TraceReport = api.TraceReport

type TraceReview = api.TraceReview

type TraceRuling = api.TraceRuling

type TraceLanding = api.TraceLanding

type TraceRebase = api.TraceRebase

type TraceTurn = api.TraceTurn

type TraceEvent = api.TraceEvent

type UnitTrace = api.UnitTrace

type TraceDelivery = api.TraceDelivery

type CriterionTrace = api.CriterionTrace

type CommitRecord = api.CommitRecord

type CommitLanding = api.CommitLanding

type CommitTrace = api.CommitTrace

const Prefix = api.Prefix

type Code = api.Code

const Malformed = api.Malformed

const Validation = api.Validation

const Conflict = api.Conflict

const Unsupported = api.Unsupported

const RestartRequired = api.RestartRequired

const ReloadRequired = api.ReloadRequired

const Unavailable = api.Unavailable

const Internal = api.Internal

const NoProject = api.NoProject

const ProjectActive = api.ProjectActive

const NotFound = api.NotFound

const CharterEmpty = api.CharterEmpty

const Forbidden = api.Forbidden

type APIError = api.APIError

type ErrorResponse = api.ErrorResponse

type Identity = api.Identity

type HealthResponse = api.HealthResponse

const BackendReady = api.BackendReady

const BackendNotReady = api.BackendNotReady

const BackendDegraded = api.BackendDegraded

const TailnetUp = api.TailnetUp

const TailnetConnecting = api.TailnetConnecting

const TailnetNeedsLogin = api.TailnetNeedsLogin

const TailnetDown = api.TailnetDown

const TailnetHTTPSOff = api.TailnetHTTPSOff

const TailnetHTTPSUp = api.TailnetHTTPSUp

const TailnetHTTPSUnavailable = api.TailnetHTTPSUnavailable

type TailnetStatus = api.TailnetStatus

type Diagnostic = api.Diagnostic

type ConfigResponse = api.ConfigResponse

const ConfigUnchanged = api.ConfigUnchanged

const ConfigChanged = api.ConfigChanged

const ConfigInvalid = api.ConfigInvalid

type ConfigDrift = api.ConfigDrift

type ConfigFile = api.ConfigFile

type ReloadResponse = api.ReloadResponse

type ReloadError = api.ReloadError

type ProjectView = api.ProjectView

type RepoView = api.RepoView

type ExtractionState = api.ExtractionState

type ProjectExtractRequest = api.ProjectExtractRequest

type ProjectRebaseRequest = api.ProjectRebaseRequest

type ProjectRebaseResponse = api.ProjectRebaseResponse

type WorkstreamRebaseResponse = api.WorkstreamRebaseResponse

type DriftCoverage = api.DriftCoverage

type DriftSkip = api.DriftSkip

type ExtractionResponse = api.ExtractionResponse

type CharterState = api.CharterState

type HandInRequest = api.HandInRequest

type HandInResponse = api.HandInResponse

type DesignOpenRequest = api.DesignOpenRequest

type DesignView = api.DesignView

type DesignStartRequest = api.DesignStartRequest

type DesignResponse = api.DesignResponse

type DesignsResponse = api.DesignsResponse

type DesignMessageRequest = api.DesignMessageRequest

type DesignStartResponse = api.DesignStartResponse

type AbandonRequest = api.AbandonRequest

type AbandonResponse = api.AbandonResponse

type MergeResponse = api.MergeResponse

type BaseUpstreamRequest = api.BaseUpstreamRequest

type BaseUpstreamResponse = api.BaseUpstreamResponse

const BaseDecisionUpstream = api.BaseDecisionUpstream

type ArchiveResponse = api.ArchiveResponse

type ShedObjectRequest = api.ShedObjectRequest

type ShedRuleRequest = api.ShedRuleRequest

type ShedOverruleRequest = api.ShedOverruleRequest

type ShedMoreRequest = api.ShedMoreRequest

type ShedRedraftRequest = api.ShedRedraftRequest

type PacketResponse = api.PacketResponse

type RatifyRequest = api.RatifyRequest

type RatifyResponse = api.RatifyResponse

type ShedResponse = api.ShedResponse

type ProjectAddRequest = api.ProjectAddRequest

type ProjectPreviewRequest = api.ProjectPreviewRequest

type ProjectPreviewResponse = api.ProjectPreviewResponse

type ProjectRemoveRequest = api.ProjectRemoveRequest

type ProjectResponse = api.ProjectResponse

type RuntimeResponse = api.RuntimeResponse

type EffectiveProfile = api.EffectiveProfile

type ProjectRuntime = api.ProjectRuntime

type PauseRequest = api.PauseRequest

type ClearPauseRequest = api.ClearPauseRequest

type PriorityRequest = api.PriorityRequest

type ClearPriorityRequest = api.ClearPriorityRequest

type ProfileRequest = api.ProfileRequest

type ClearProfileRequest = api.ClearProfileRequest

type ClearProviderLimitRequest = api.ClearProviderLimitRequest

type MutationResponse = api.MutationResponse

type StopResponse = api.StopResponse

type StatusResponse = api.StatusResponse

type WorkspacesStatus = api.WorkspacesStatus

type FailureStreak = api.FailureStreak

type DailyBudgetStatus = api.DailyBudgetStatus

type CapacityStatus = api.CapacityStatus

type RoleCapacity = api.RoleCapacity

type SlotWait = api.SlotWait

type ProviderUsageStatus = api.ProviderUsageStatus

type ProviderSpend = api.ProviderSpend

type ProviderUsage = api.ProviderUsage

type RoleFallback = api.RoleFallback

type WorkstreamStatus = api.WorkstreamStatus

type WorkgroupView = api.WorkgroupView

type SessionOpenRequest = api.SessionOpenRequest

type SessionView = api.SessionView

type SessionResponse = api.SessionResponse

type SessionDestroyResponse = api.SessionDestroyResponse

type SessionsResponse = api.SessionsResponse

type SessionMessageRequest = api.SessionMessageRequest

type SessionCommitSummary = api.SessionCommitSummary

type SessionChangesResponse = api.SessionChangesResponse

type SessionCommitRequest = api.SessionCommitRequest

type SessionCommitResponse = api.SessionCommitResponse

type SessionCheckRun = api.SessionCheckRun

type SessionPushResponse = api.SessionPushResponse

type AgentStatus = api.AgentStatus

type DriftStatus = api.DriftStatus

type StatusView = api.StatusView

type InboxResponse = api.InboxResponse

const InboxEscalation = api.InboxEscalation

const InboxRatification = api.InboxRatification

const InboxContested = api.InboxContested

const InboxAmendment = api.InboxAmendment

const InboxDelivery = api.InboxDelivery

const InboxPublication = api.InboxPublication

const InboxFinalChecks = api.InboxFinalChecks

const InboxDrift = api.InboxDrift

const InboxNotices = api.InboxNotices

const InboxLoop = api.InboxLoop

const InboxBase = api.InboxBase

type InboxEntry = api.InboxEntry

type InboxAnswer = api.InboxAnswer

type InboxQuestion = api.InboxQuestion

type AnswerRequest = api.AnswerRequest

type AnswerResponse = api.AnswerResponse

type SendRequest = api.SendRequest

type TurnState = api.TurnState

const TurnQueued = api.TurnQueued

const TurnRunning = api.TurnRunning

const TurnDone = api.TurnDone

const TurnFailed = api.TurnFailed

const TurnStopped = api.TurnStopped

type ConversationResponse = api.ConversationResponse

type ConversationEntry = api.ConversationEntry

type FeedResponse = api.FeedResponse

type FeedEntry = api.FeedEntry

type FeedSession = api.FeedSession

type FeedTransition = api.FeedTransition

type FeedCheckSelection = api.FeedCheckSelection

type FeedUnitChecks = api.FeedUnitChecks

type FeedFinalChecks = api.FeedFinalChecks

type FeedFinalReview = api.FeedFinalReview

type EventKind = api.EventKind

const EventResync = api.EventResync

const EventWorkstream = api.EventWorkstream

const EventInbox = api.EventInbox

const EventConversation = api.EventConversation

const EventRuntime = api.EventRuntime

const EventConfig = api.EventConfig

const EventSpend = api.EventSpend

const EventActivity = api.EventActivity

const EventCapacity = api.EventCapacity

type Event = api.Event

type UnitMoveRequest = api.UnitMoveRequest

type UnitMove = api.UnitMove

type UnitMoveResponse = api.UnitMoveResponse

type WorkstreamMoveRequest = api.WorkstreamMoveRequest

type WorkstreamMove = api.WorkstreamMove

type WorkstreamMoveResponse = api.WorkstreamMoveResponse

type WorkgroupFeedResponse = api.WorkgroupFeedResponse
type WorkgroupMemberSummary = api.WorkgroupMemberSummary
