// Package delivery runs a workstream's final checks and review and publishes
// or lands the assembled feature branch.
package delivery

import (
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
)

type APIError = api.APIError

const AssembledState = intake.AssembledState

const BuildingState = intake.BuildingState

type CheckResult = api.CheckResult

const ChecksIncomplete = execution.ChecksIncomplete

const Conflict = api.Conflict

const DeliveredState = intake.DeliveredState

const BriefNoneRecorded = api.BriefNoneRecorded

const BriefRecorded = api.BriefRecorded

const BriefWaiting = api.BriefWaiting

type DeliveryApproval = api.DeliveryApproval

// DeliveryRouteLand is the route of an approval to land the workstream on the
// base branch.
const DeliveryRouteLand = api.DeliveryRouteLand

type DeliveryLandDecision = api.DeliveryLandDecision

type DeliveryLandResponse = api.DeliveryLandResponse

type DeliveryChecks = api.DeliveryChecks

type DeliveryReady = api.DeliveryReady

type DeliveryBrief = api.DeliveryBrief

type DeliveryDecision = api.DeliveryDecision

type DeliveryMerge = api.DeliveryMerge

type DeliveryPresentation = api.DeliveryPresentation

type DeliveryPublication = api.DeliveryPublication

const DriftAction = execution.DriftAction

type FinalCriterion = api.FinalCriterion

type FinalFinding = api.FinalFinding

type FinalReport = api.FinalReport

const InboxDelivery = api.InboxDelivery

type InboxEntry = api.InboxEntry

type InboxAnswer = api.InboxAnswer

const InboxFinalChecks = api.InboxFinalChecks

const InboxChecksChoice = api.InboxChecksChoice

type ChecksChoiceRequest = api.ChecksChoiceRequest

type ChecksChoiceResponse = api.ChecksChoiceResponse

const InboxPublication = api.InboxPublication

const Internal = api.Internal

type MergeResponse = api.MergeResponse

const UnitMerged = intake.UnitMerged

const UnitPlanned = intake.UnitPlanned

const UnitReady = intake.UnitReady

const Validation = api.Validation

var abandonActor = intake.AbandonActor

var chiefActor = intake.ChiefActor

var abandoned = intake.Abandoned

const cancelReason = intake.CancelReason

const checkRetryDelay = execution.CheckRetryDelay

var checksActor = execution.ChecksActor

var committeeAgent = intake.CommitteeAgent

const committeeRole = intake.CommitteeRole

var continuedTurn = intake.ContinuedTurn

var decision = intake.Decision

var errChangeBoundary = intake.ErrChangeBoundary

var errNoCommittee = intake.ErrNoCommittee

var featureBranch = intake.FeatureBranch

var featureState = intake.FeatureState

var featureWorkspaces = intake.FeatureWorkspaces

var foremanActor = execution.ForemanActor

const handedDocument = intake.HandedDocument

var isContinuation = intake.IsContinuation

var isSession = intake.IsSession

var landingDocument = execution.LandingDocument

const maxFinalRounds = intake.MaxFinalRounds

const maxIncompleteChecks = execution.MaxIncompleteChecks

const maxRoundAttempts = intake.MaxRoundAttempts

var nextRevision = intake.NextRevision

var ownerActor = intake.OwnerActor

var ownerHeader = intake.OwnerHeader

const publicationEscalation = intake.PublicationEscalation

var refuseReport = intake.RefuseReport

var reportDocument = execution.ReportDocument

type stagedWorkspaces = intake.StagedWorkspaces

var stoppedTurn = intake.StoppedTurn

const subjectLength = execution.SubjectLength

const deliveryReviewAction = "delivery-review"
const publishUpstreamAction = "publish-upstream"
const moveWorkstreamTool = "move_workstream"

var nonSessionWorkstreams = intake.NonSessionWorkstreams
