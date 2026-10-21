package service

// Test-prefixed exports in this file are test-only seams for running-service fixtures.

const TestBaseRefreshAction = baseRefreshAction

var TestChiefActions = chiefActions
var TestChiefContestPath = chiefContestPath
var TestChiefOrdinaryTools = chiefOrdinaryTools
var TestConcurrentOperation = concurrentOperation
var TestDecodeFinalReview = decodeFinalReview
var TestDecodeLand = decodeLand

var TestEdgeObservationAt = edgeObservationAt

const TestEdgePoll = edgePoll

var TestEdgeWaiting = edgeWaiting

const TestEscalateContest = escalateContest
const TestFinalReportDocument = finalReportDocument
const TestFinalReportPath = finalReportPath

var TestFinalReviewActor = finalReviewActor

const TestFinalReviewed = finalReviewed

var TestFinalTurnID = finalTurnID
var TestForemanActor = foremanActor
var TestLandIDs = landIDs

type TestLandInput = landInput

var TestLeastWorkstream = leastWorkstream
var TestLibrarianActor = librarianActor

const TestMasonDone = masonDone

var TestMasonTurnID = masonTurnID
var TestNextReadyTurn = nextReadyTurn
var TestProjectContext = projectContext

const TestProjectContextTool = projectContextTool

type TestPublishInput = publishInput
type TestReportingTurns = reportingTurns
type TestRetriedTurns = retriedTurns

var TestReviewDocument = reviewDocument
var TestReviewTurnID = reviewTurnID
var TestReviewerActor = reviewerActor

var TestUnitContest = unitContest

type TestVerdictTurns = verdictTurns

var TestStageChiefDocuments = stageChiefDocuments
var TestWithActions = withActions
var TestDriftResolveTurnID = driftResolveTurnID
var TestDriftReviewTurnID = driftReviewTurnID
var TestDriftDiffTool = driftDiffTool
var TestDriftWorkspaces = driftWorkspaces
var TestOperationTrailer = operationTrailer
var TestErrDriftAwaits = errDriftAwaits

const TestDriftRebased = driftRebased
const TestCommitChange = commitChange
const TestDriftDocument = driftDocument
const TestDriftMasonAgent = driftMasonAgent
const TestDriftReviewerAgent = driftReviewerAgent
const TestDriftsDirectory = driftsDirectory
const TestLibrarianAgent = librarianAgent

type TestRefreshInput = refreshInput

var TestContestOptions = contestOptions

const TestTurnRetries = turnRetries

var TestFeedChecks = feedChecks
var TestFeedSelection = feedSelection
