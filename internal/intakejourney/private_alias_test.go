package intakejourney

import (
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
)

const CheckAction = service.CheckAction

type Enforcement = service.Enforcement

const FinalReportTool = service.FinalReportTool
const FinalReviewAction = service.FinalReviewAction

const LandAction = service.LandAction

type MasonReport = service.MasonReport

const ProjectSpecDraftTool = service.ProjectSpecDraftTool

type UnitReviewResult = service.UnitReviewResult
type UnitVerdict = service.UnitVerdict

var chiefActions = service.TestChiefActions
var chiefContestPath = execution.ChiefContestPath
var chiefOrdinaryTools = service.TestChiefOrdinaryTools
var concurrentOperation = service.TestConcurrentOperation
var decodeFinalReview = delivery.DecodeFinalReview
var decodeLand = execution.DecodeLand

var edgeObservationAt = lifecycle.EdgeObservationAt

const edgePoll = lifecycle.EdgePoll

var edgeWaiting = service.TestEdgeWaiting

const escalateContest = execution.EscalateContest
const finalReportDocument = service.TestFinalReportDocument
const finalReportPath = service.TestFinalReportPath

var finalReviewActor = service.TestFinalReviewActor

const finalReviewed = service.TestFinalReviewed

var finalTurnID = service.TestFinalTurnID
var foremanActor = service.TestForemanActor
var landIDs = execution.LandIDs

type landInput = service.TestLandInput

var leastWorkstream = lifecycle.LeastWorkstream

var masonTurnID = service.TestMasonTurnID

var nextReadyTurn = lifecycle.NextReadyTurn

const projectContextTool = service.TestProjectContextTool

type publishInput = service.TestPublishInput
type reportingTurns = service.TestReportingTurns
type retriedTurns = service.TestRetriedTurns

var reviewDocument = service.TestReviewDocument
var reviewTurnID = service.TestReviewTurnID
var reviewerActor = execution.ReviewerActor

var unitContest = service.TestUnitContest

type verdictTurns = service.TestVerdictTurns
