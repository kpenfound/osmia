package intakejourney

import "github.com/kpenfound/osmia/internal/service"

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
var chiefContestPath = service.TestChiefContestPath
var chiefOrdinaryTools = service.TestChiefOrdinaryTools
var concurrentOperation = service.TestConcurrentOperation
var decodeFinalReview = service.TestDecodeFinalReview
var decodeLand = service.TestDecodeLand

var edgeObservationAt = service.TestEdgeObservationAt

const edgePoll = service.TestEdgePoll

var edgeWaiting = service.TestEdgeWaiting

const escalateContest = service.TestEscalateContest
const finalReportDocument = service.TestFinalReportDocument
const finalReportPath = service.TestFinalReportPath

var finalReviewActor = service.TestFinalReviewActor

const finalReviewed = service.TestFinalReviewed

var finalTurnID = service.TestFinalTurnID
var foremanActor = service.TestForemanActor
var landIDs = service.TestLandIDs

type landInput = service.TestLandInput

var leastWorkstream = service.TestLeastWorkstream
var librarianActor = service.TestLibrarianActor

var masonTurnID = service.TestMasonTurnID

var nextReadyTurn = service.TestNextReadyTurn
var projectContext = service.TestProjectContext

const projectContextTool = service.TestProjectContextTool

type publishInput = service.TestPublishInput
type reportingTurns = service.TestReportingTurns
type retriedTurns = service.TestRetriedTurns

var reviewDocument = service.TestReviewDocument
var reviewTurnID = service.TestReviewTurnID
var reviewerActor = service.TestReviewerActor

var unitContest = service.TestUnitContest

type verdictTurns = service.TestVerdictTurns
