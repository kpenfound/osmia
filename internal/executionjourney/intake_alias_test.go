package executionjourney

import (
	"github.com/kpenfound/osmia/internal/intake"
)

const DeliveredState = intake.DeliveredState

const cancelReason = intake.CancelReason

var abandonActor = intake.AbandonActor

const BuildingState = intake.BuildingState

const UnitPlanned = intake.UnitPlanned
const UnitReady = intake.UnitReady
const UnitImplementing = intake.UnitImplementing

const UnitReviewing = intake.UnitReviewing
const UnitApproved = intake.UnitApproved
const UnitMerged = intake.UnitMerged
const UnitWaiting = intake.UnitWaiting

const UnitContested = intake.UnitContested

type builder = intake.Builder

var chiefActor = intake.ChiefActor

var ownerActor = intake.OwnerActor

const AssembledState = intake.AssembledState

var featureWorkspaces = intake.FeatureWorkspaces

var masonActor = intake.MasonActor
var masonAgent = intake.MasonAgent

var serviceActor = intake.ServiceActor

const reviewerRole = intake.ReviewerRole

var reviewerAgent = intake.ReviewerAgent

const branchesDirectory = intake.BranchesDirectory

var featureBranch = intake.FeatureBranch

type Committee = intake.Committee

const masonRole = intake.MasonRole

const unitsDirectory = intake.UnitsDirectory

var workspaces = intake.Workspaces
