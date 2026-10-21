package executionjourney

import "github.com/kpenfound/osmia/internal/api"

type APIError = api.APIError
type ActivityTurn = api.ActivityTurn
type CapacityStatus = api.CapacityStatus
type CheckResult = api.CheckResult
type CheckSelection = api.CheckSelection
type ClearPauseRequest = api.ClearPauseRequest
type ClearPriorityRequest = api.ClearPriorityRequest
type ClearProfileRequest = api.ClearProfileRequest
type CommitRecord = api.CommitRecord
type CommitTrace = api.CommitTrace
type ContestedRuling = api.ContestedRuling
type ContestedRulingRequest = api.ContestedRulingRequest
type ContestedRulingResponse = api.ContestedRulingResponse
type ConversationEntry = api.ConversationEntry
type DeliveryDecision = api.DeliveryDecision
type DeliveryPresentation = api.DeliveryPresentation
type DriftCoverage = api.DriftCoverage
type EffectiveProfile = api.EffectiveProfile
type FeedResponse = api.FeedResponse

const InboxContested = api.InboxContested

type InboxEntry = api.InboxEntry
type MutationResponse = api.MutationResponse
type OverlapAdvisory = api.OverlapAdvisory
type PacketResponse = api.PacketResponse
type PauseRequest = api.PauseRequest
type PriorityRequest = api.PriorityRequest
type ProfileRequest = api.ProfileRequest
type ProjectRebaseResponse = api.ProjectRebaseResponse
type ReviewFinding = api.ReviewFinding
type RoleCapacity = api.RoleCapacity
type SessionCheckRun = api.SessionCheckRun
type SlotWait = api.SlotWait
type UnitActivity = api.UnitActivity
type UnitDispatch = api.UnitDispatch
type UnitLanding = api.UnitLanding
type UnitMoveRequest = api.UnitMoveRequest
type UnitMoveResponse = api.UnitMoveResponse
type UnitRebase = api.UnitRebase
type UnitStatus = api.UnitStatus

const Validation = api.Validation

type WorkspacesStatus = api.WorkspacesStatus
type WorkstreamStatus = api.WorkstreamStatus
