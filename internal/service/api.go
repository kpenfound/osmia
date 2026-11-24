package service

import "github.com/kpenfound/osmia/internal/api"

type ActivityKind = api.ActivityKind

const ActivityCheck = api.ActivityCheck

const ActivityAgentTurn = api.ActivityAgentTurn

const ActivityRecovery = api.ActivityRecovery

type ActivityEntry = api.ActivityEntry

type ActivityResponse = api.ActivityResponse

type AmendmentDecisionRequest = api.AmendmentDecisionRequest

type AmendmentResponse = api.AmendmentResponse

type UnitStatus = api.UnitStatus

type CharterDecisionRequest = api.CharterDecisionRequest

type CharterProposalView = api.CharterProposalView

type UnitActivity = api.UnitActivity

type ActivityTransition = api.ActivityTransition

type ActivityTurn = api.ActivityTurn

type ActivityRefusal = api.ActivityRefusal

type ChiefContestDecision = api.ChiefContestDecision

type DeliveryPresentation = api.DeliveryPresentation

type DeliveryApproval = api.DeliveryApproval

type DeliveryDecision = api.DeliveryDecision

type DeliveryLandDecision = api.DeliveryLandDecision

type DeliveryLandResponse = api.DeliveryLandResponse

type UnitDispatch = api.UnitDispatch

type CharterEdit = api.CharterEdit

type FinalReport = api.FinalReport

type UnitLanding = api.UnitLanding

type MemorySetup = api.MemorySetup

type DeliveryMerge = api.DeliveryMerge

type OverlapAdvisory = api.OverlapAdvisory

type DeliveryPublication = api.DeliveryPublication

type UnitRebase = api.UnitRebase

type CheckResult = api.CheckResult

type ContestedRulingRequest = api.ContestedRulingRequest

type ContestedRulingResponse = api.ContestedRulingResponse

type CheckSelection = api.CheckSelection

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

const Forbidden = api.Forbidden

const TranscriptionOff = api.TranscriptionOff

const TranscriptionFailed = api.TranscriptionFailed

const TooLarge = api.TooLarge

type TranscribeRequest = api.TranscribeRequest

type TranscribeResponse = api.TranscribeResponse

type APIError = api.APIError

type ErrorResponse = api.ErrorResponse

type Identity = api.Identity

type HealthResponse = api.HealthResponse

const TailnetUp = api.TailnetUp

const TailnetNeedsLogin = api.TailnetNeedsLogin

const TailnetDown = api.TailnetDown

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

type ProjectConfigResponse = api.ProjectConfigResponse

const ProjectConfigApplied = api.ProjectConfigApplied

const ProjectConfigSavedNotApplied = api.ProjectConfigSavedNotApplied

type ProjectConfigSetting = api.ProjectConfigSetting

type ProjectConfigEdit = api.ProjectConfigEdit

type ProjectConfigEditValues = api.ProjectConfigValues

type ProjectConfigSaveResponse = api.ProjectConfigSaveResponse

type RepoView = api.RepoView

type CharterState = api.CharterState

type HandInRequest = api.HandInRequest

type HandInResponse = api.HandInResponse

type DesignResponse = api.DesignResponse

type DesignsResponse = api.DesignsResponse

type AbandonRequest = api.AbandonRequest

type AbandonResponse = api.AbandonResponse

type ShedMoreRequest = api.ShedMoreRequest

type ShedRedraftRequest = api.ShedRedraftRequest

type RatifyRequest = api.RatifyRequest

type RatifyResponse = api.RatifyResponse

type ShedResponse = api.ShedResponse

type ProjectAddRequest = api.ProjectAddRequest

type ProjectCreateRequest = api.ProjectCreateRequest

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

type StatusView = api.StatusView

type InboxResponse = api.InboxResponse

const InboxEscalation = api.InboxEscalation

const InboxRatification = api.InboxRatification

const InboxDrift = api.InboxDrift

const InboxNotices = api.InboxNotices

const InboxLoop = api.InboxLoop

const InboxBase = api.InboxBase

type InboxEntry = api.InboxEntry

type AnswerRequest = api.AnswerRequest

type SendRequest = api.SendRequest

const TurnQueued = api.TurnQueued

const TurnDone = api.TurnDone

const TurnFailed = api.TurnFailed

type ConversationResponse = api.ConversationResponse

type ConversationEntry = api.ConversationEntry

type FeedResponse = api.FeedResponse

type FeedEntry = api.FeedEntry

type FeedSession = api.FeedSession

type FeedCheckSelection = api.FeedCheckSelection

type FeedUnitChecks = api.FeedUnitChecks

type FeedFinalChecks = api.FeedFinalChecks

const EventResync = api.EventResync

const EventWorkstream = api.EventWorkstream

const EventInbox = api.EventInbox

const EventRuntime = api.EventRuntime

const EventConfig = api.EventConfig

const EventSpend = api.EventSpend

const EventCapacity = api.EventCapacity

type Event = api.Event

type UnitMoveRequest = api.UnitMoveRequest

type WorkstreamMoveRequest = api.WorkstreamMoveRequest

type WorkgroupFeedResponse = api.WorkgroupFeedResponse
type WorkgroupMemberSummary = api.WorkgroupMemberSummary
