package service

import (
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

var recordActivity = fixturekit.RecordActivity

const handedDesign = intakefixture.HandedDesign

const validSpec = intakefixture.ValidSpec

const validPlan = intakefixture.ValidPlan

const cyclicPlan = fixturekit.CyclicPlan

var queueRoleTurn = fixturekit.QueueRoleTurn

const demoAgent = intakefixture.DemoAgent

const demoThread = intakefixture.DemoThread

const demoRole = fixturekit.DemoRole

const demoSecret = intakefixture.DemoSecret

var demoStart = fixturekit.DemoStart

const demoTimeout = intakefixture.DemoTimeout

type demoClock = fixturekit.DemoClock

type demoWorkspaces = fixturekit.DemoWorkspaces

type demoTransport = fixturekit.DemoTransport

type demoSessions = fixturekit.DemoSessions

type demoTurn = intakefixture.DemoTurn

type demoEngine = fixturekit.DemoEngine

var callTool = intakefixture.CallTool

var demoGit = fixturekit.DemoGit

type chiefTurns = fixturekit.ChiefTurns

var jump = fixturekit.Jump

type fakeTurn = intakefixture.FakeTurn

type fixedClock = fixturekit.FixedClock

type fakeIssues = fixturekit.FakeIssues

const masonWrote = intakefixture.MasonWrote

const commentedConfig = fixturekit.CommentedConfig

var request = fixturekit.Request

type fakePulls = fixturekit.FakePulls

var configureDeliverySigner = fixturekit.ConfigureDeliverySigner

var claimTurn = fixturekit.ClaimTurn

type checkFunc = fixturekit.CheckFunc

var passingChecks = fixturekit.PassingChecks

const project = intakefixture.Project

const stream = intakefixture.Stream

var must = intakefixture.Must

var traceQuery = intakefixture.TraceQuery

var traceExec = fixturekit.TraceExec

var workstreamTraceDirectory = intakefixture.WorkstreamTraceDirectory

var workstreamRecords = intakefixture.WorkstreamRecords

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

type fakeJJ = fixturekit.FakeJJ

func testOptions(cfg config.Options) Options {
	return Options{Config: cfg, Build: Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{stream}, ShutdownTimeout: 100 * time.Millisecond}
}

type blockingTurns = fixturekit.BlockingTurns

var pauseOf = fixturekit.PauseOf

type completedRunner = fixturekit.CompletedRunner

type faults = intakefixture.Faults

var amendmentOp = intakefixture.AmendmentOp

var apiError = intakefixture.ApiError

var passing = fixturekit.Passing
