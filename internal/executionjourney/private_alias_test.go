package executionjourney

import "github.com/kpenfound/osmia/internal/service"

const CheckAction = service.CheckAction

type Enforcement = service.Enforcement

const FinalReportTool = service.FinalReportTool

const LandAction = service.LandAction

type MasonReport = service.MasonReport

type UnitReviewResult = service.UnitReviewResult
type UnitVerdict = service.UnitVerdict

var chiefActions = service.TestChiefActions
var chiefContestPath = service.TestChiefContestPath

var decodeLand = service.TestDecodeLand

var edgeObservationAt = service.TestEdgeObservationAt

const edgePoll = service.TestEdgePoll

var edgeWaiting = service.TestEdgeWaiting

const escalateContest = service.TestEscalateContest

var foremanActor = service.TestForemanActor
var landIDs = service.TestLandIDs

type landInput = service.TestLandInput

var leastWorkstream = service.TestLeastWorkstream
var librarianActor = service.TestLibrarianActor

var masonTurnID = service.TestMasonTurnID

var nextReadyTurn = service.TestNextReadyTurn

type reportingTurns = service.TestReportingTurns
type retriedTurns = service.TestRetriedTurns

var reviewDocument = service.TestReviewDocument
var reviewTurnID = service.TestReviewTurnID
var reviewerActor = service.TestReviewerActor

var unitContest = service.TestUnitContest

type verdictTurns = service.TestVerdictTurns
