package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const DeliveredState = intake.DeliveredState
const cancelReason = intake.CancelReason

var abandonActor = intake.AbandonActor

const BuildingState = intakefixture.BuildingState

const UnitReady = intake.UnitReady
const UnitImplementing = intakefixture.UnitImplementing
const UnitReviewing = intake.UnitReviewing
const UnitApproved = intake.UnitApproved
const UnitMerged = intake.UnitMerged
const UnitWaiting = intakefixture.UnitWaiting

type builder = intake.Builder

var chiefActor = intakefixture.ChiefActor
var ownerActor = intake.OwnerActor
var librarianWorkstream = intake.LibrarianWorkstream

const AssembledState = intake.AssembledState

var featureWorkspaces = intake.FeatureWorkspaces
var masonAgent = intakefixture.MasonAgent
var serviceActor = intake.ServiceActor

const reviewerRole = intakefixture.ReviewerRole

var reviewerAgent = intake.ReviewerAgent

const branchesDirectory = intake.BranchesDirectory

var featureBranch = intake.FeatureBranch

type Committee = intake.Committee

const masonRole = intakefixture.MasonRole
const unitsDirectory = intake.UnitsDirectory

var workspaces = intake.Workspaces
