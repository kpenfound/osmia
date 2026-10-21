package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/api"
)

type DeliveryPresentation = api.DeliveryPresentation
type DeliveryDecision = api.DeliveryDecision

type UnitLanding = api.UnitLanding

type UnitRebase = api.UnitRebase
type CheckResult = api.CheckResult
type ReviewFinding = api.ReviewFinding

const Prefix = api.Prefix
const Validation = api.Validation

type APIError = api.APIError
type ProjectRebaseResponse = api.ProjectRebaseResponse
type DriftCoverage = api.DriftCoverage
type PacketResponse = api.PacketResponse
type PauseRequest = api.PauseRequest
type WorkstreamStatus = api.WorkstreamStatus
type InboxEntry = api.InboxEntry
type ConversationEntry = api.ConversationEntry
type FeedResponse = api.FeedResponse
