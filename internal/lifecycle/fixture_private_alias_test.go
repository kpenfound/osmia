package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/service"
)

const CheckAction = service.CheckAction

type Enforcement = service.Enforcement

const FinalReportTool = service.FinalReportTool
const LandAction = service.LandAction

type MasonReport = service.MasonReport
type UnitReviewResult = service.UnitReviewResult
type UnitVerdict = service.UnitVerdict

var chiefActions = service.TestChiefActions
var decodeLand = service.TestDecodeLand
var foremanActor = service.TestForemanActor
var landIDs = service.TestLandIDs

type landInput = service.TestLandInput

var librarianActor = service.TestLibrarianActor
var masonTurnID = service.TestMasonTurnID

type reportingTurns = service.TestReportingTurns
type retriedTurns = service.TestRetriedTurns

var reviewDocument = service.TestReviewDocument
var reviewTurnID = service.TestReviewTurnID
var reviewerActor = service.TestReviewerActor

type verdictTurns = service.TestVerdictTurns
