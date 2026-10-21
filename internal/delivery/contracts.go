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

type CheckSelection = api.CheckSelection

const checksRole = execution.ChecksRole

const ChecksIncomplete = execution.ChecksIncomplete

const Conflict = api.Conflict

const DeliveredState = intake.DeliveredState

type DeliveryApproval = api.DeliveryApproval

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

const InboxFinalChecks = api.InboxFinalChecks

const InboxPublication = api.InboxPublication

const Internal = api.Internal

type MergeResponse = api.MergeResponse

const UnitMerged = intake.UnitMerged

const UnitPlanned = intake.UnitPlanned

const UnitReady = intake.UnitReady

const Validation = api.Validation

var abandonActor = intake.AbandonActor

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

var librarianWorkstream = intake.LibrarianWorkstream

const maxFinalRounds = intake.MaxFinalRounds

const maxIncompleteChecks = execution.MaxIncompleteChecks

const maxRoundAttempts = intake.MaxRoundAttempts

var nextRevision = intake.NextRevision

var ownerActor = intake.OwnerActor

var ownerHeader = intake.OwnerHeader

const publicationEscalation = intake.PublicationEscalation

var quoteLinks = execution.QuoteLinks

var refuseReport = intake.RefuseReport

var reportDocument = execution.ReportDocument

type stagedWorkspaces = intake.StagedWorkspaces

var stoppedTurn = intake.StoppedTurn

const subjectLength = execution.SubjectLength

const deliveryReviewAction = "delivery-review"
const publishUpstreamAction = "publish-upstream"
const moveWorkstreamTool = "move_workstream"

const checkGateReviewInstructions = " Test passing is decided by the service-owned check gate. When the gate passed for the candidate, every acceptance item or criterion requiring tests to pass (whole-suite, scoped or named) is satisfied as to passing: the passing gate is the evidence that required tests ran and passed. Never raise a finding by itself because only a smart-check selection ran, because you cannot see that a named test was selected, or because you cannot run tests yourself. Still judge from the work whether tests the unit was asked to add exist and exercise the stated behaviour."
