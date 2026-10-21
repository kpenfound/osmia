package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

var recordActivity = fixturekit.RecordActivity

const handedDesign = fixturekit.HandedDesign

const validSpec = fixturekit.ValidSpec

const validPlan = fixturekit.ValidPlan

const cyclicPlan = fixturekit.CyclicPlan

var queueRoleTurn = fixturekit.QueueRoleTurn

const demoAgent = fixturekit.DemoAgent

const demoThread = fixturekit.DemoThread

const demoRole = fixturekit.DemoRole

const demoSecret = fixturekit.DemoSecret

var demoStart = fixturekit.DemoStart

const demoTimeout = fixturekit.DemoTimeout

type demoClock = fixturekit.DemoClock

type demoWorkspaces = fixturekit.DemoWorkspaces

type demoTransport = fixturekit.DemoTransport

type demoSessions = fixturekit.DemoSessions

type demoTurn = fixturekit.DemoTurn

type demoEngine = fixturekit.DemoEngine

var callTool = fixturekit.CallTool

var demoGit = fixturekit.DemoGit

type chiefTurns = fixturekit.ChiefTurns

var jump = fixturekit.Jump

type fakeTurn = fixturekit.FakeTurn

type fixedClock = fixturekit.FixedClock

type fakeIssues = fixturekit.FakeIssues

const masonWrote = fixturekit.MasonWrote

const commentedConfig = fixturekit.CommentedConfig

var request = fixturekit.Request

type fakePulls = fixturekit.FakePulls

var configureDeliverySigner = fixturekit.ConfigureDeliverySigner

var claimTurn = fixturekit.ClaimTurn

type checkFunc = fixturekit.CheckFunc

var passingChecks = fixturekit.PassingChecks

const project = fixturekit.Project

const stream = fixturekit.Stream

var must = fixturekit.Must

var traceQuery = fixturekit.TraceQuery

var traceExec = fixturekit.TraceExec

var workstreamTraceDirectory = fixturekit.WorkstreamTraceDirectory

var workstreamRecords = fixturekit.WorkstreamRecords

var latestDocumentIn = fixturekit.LatestDocumentIn

var workstreamDocument = fixturekit.WorkstreamDocument

var projectDocument = fixturekit.ProjectDocument

var traceCommits = fixturekit.TraceCommits

var traceFile = fixturekit.TraceFile

var recordCharter = fixturekit.RecordCharter

type sessionEngine = fixturekit.SessionEngine

type checkOutcome = fixturekit.CheckOutcome

type fakeChecks = fixturekit.FakeChecks

const passedReport = fixturekit.PassedReport

var failedReport = fixturekit.FailedReport

var passing = fixturekit.Passing

type fakeJJ = fixturekit.FakeJJ

func testOptions(cfg config.Options) Options {
	return Options{Config: cfg, Build: Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{stream}, ShutdownTimeout: 100 * time.Millisecond}
}

type blockingTurns = fixturekit.BlockingTurns

var pauseOf = fixturekit.PauseOf

type completedRunner = fixturekit.CompletedRunner
