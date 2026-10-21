package execution

import (
	api "github.com/kpenfound/osmia/internal/api"
	checks "github.com/kpenfound/osmia/internal/checks"
	intake "github.com/kpenfound/osmia/internal/intake"
	reviewtools "github.com/kpenfound/osmia/internal/review"
)

const AssembledState = intake.AssembledState
const BuildingState = intake.BuildingState
const ChecksFailed = checks.ChecksFailed
const ChecksIncomplete = checks.ChecksIncomplete
const ChecksPassed = checks.ChecksPassed
const ChecksSuperseded = checks.ChecksSuperseded
const Conflict = api.Conflict
const DeliveredState = intake.DeliveredState
const Internal = api.Internal
const NoProject = api.NoProject
const NotFound = api.NotFound
const UnitApproved = intake.UnitApproved
const UnitChecking = intake.UnitChecking
const UnitContested = intake.UnitContested
const UnitImplementing = intake.UnitImplementing
const UnitMerged = intake.UnitMerged
const UnitPlanned = intake.UnitPlanned
const UnitReady = intake.UnitReady
const UnitReviewing = intake.UnitReviewing
const UnitWaiting = intake.UnitWaiting
const Validation = api.Validation
const cancelReason = intake.CancelReason
const driftMasonAgent = intake.DriftMasonAgent
const masonRole = intake.MasonRole
const reviewerRole = intake.ReviewerRole
const unitsDirectory = intake.UnitsDirectory
const driftsDirectory = intake.DriftsDirectory

type APIError = api.APIError
type CheckSelection = api.CheckSelection
type ChiefContestDecision = api.ChiefContestDecision
type Code = api.Code
type ContestedRuling = api.ContestedRuling
type ContestedRulingRequest = api.ContestedRulingRequest
type ContestedRulingResponse = api.ContestedRulingResponse
type Diagnostic = api.Diagnostic
type Event = api.Event
type Identity = api.Identity
type ReviewChecks = checks.ReviewChecks
type ReviewFinding = api.ReviewFinding
type UnitDispatch = api.UnitDispatch
type UnitLanding = api.UnitLanding
type UnitMove = api.UnitMove
type UnitMoveRequest = api.UnitMoveRequest
type UnitMoveResponse = api.UnitMoveResponse
type UnitRebase = api.UnitRebase
type UnitReviewIdentity = reviewtools.UnitReviewIdentity
type noLease = intake.NoLease
type streamWorkspaces = intake.StreamWorkspaces

var abandonActor = intake.AbandonActor
var abandoned = intake.Abandoned
var askedBy = intake.AskedBy
var chiefActor = intake.ChiefActor
var errChangeBoundary = intake.ErrChangeBoundary
var featureBranch = intake.FeatureBranch
var featureState = intake.FeatureState
var featureWorkspaces = intake.FeatureWorkspaces
var isSession = intake.IsSession
var landingCriteria = intake.LandingCriteria
var librarianWorkstream = intake.LibrarianWorkstream
var masonActor = intake.MasonActor
var masonAgent = intake.MasonAgent
var masonSystemPrompt = intake.MasonSystemPrompt
var ownerActor = intake.OwnerActor
var priorityRefusal = intake.PriorityRefusal
var refuseReport = intake.RefuseReport
var reviewerAgent = intake.ReviewerAgent
var sealedPlan = intake.SealedPlan
var sealedUnit = intake.SealedUnit

const DriftAction = "drift"

const reviewInputsDirectory = "review-inputs"
const checkGateReviewInstructions = " Test passing is decided by the service-owned check gate. When the gate passed for the candidate, every acceptance item or criterion requiring tests to pass (whole-suite, scoped or named) is satisfied as to passing: the passing gate is the evidence that required tests ran and passed. Never raise a finding by itself because only a smart-check selection ran, because you cannot see that a named test was selected, or because you cannot run tests yourself. Still judge from the work whether tests the unit was asked to add exist and exercise the stated behaviour."
