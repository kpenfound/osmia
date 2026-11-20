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

var queueRoleTurn = fixturekit.QueueRoleTurn

const demoAgent = intakefixture.DemoAgent

const demoThread = intakefixture.DemoThread

const demoRole = fixturekit.DemoRole

var demoStart = fixturekit.DemoStart

const demoTimeout = intakefixture.DemoTimeout

type demoClock = fixturekit.DemoClock

type demoTransport = fixturekit.DemoTransport

type demoSessions = fixturekit.DemoSessions

type demoTurn = intakefixture.DemoTurn

type demoEngine = fixturekit.DemoEngine

var callTool = intakefixture.CallTool

var demoGit = fixturekit.DemoGit

type chiefTurns = fixturekit.ChiefTurns

var jump = fixturekit.Jump

type fixedClock = fixturekit.FixedClock

type fakeIssues = fixturekit.FakeIssues

const masonWrote = intakefixture.MasonWrote

var request = fixturekit.Request

type fakePulls = fixturekit.FakePulls

var passingChecks = fixturekit.PassingChecks

const project = intakefixture.Project

const stream = intakefixture.Stream

var must = intakefixture.Must

var workstreamTraceDirectory = intakefixture.WorkstreamTraceDirectory

var workstreamDocument = fixturekit.WorkstreamDocument

var traceCommits = fixturekit.TraceCommits

type checkOutcome = fixturekit.CheckOutcome

type fakeChecks = fixturekit.FakeChecks

type fakeJJ = fixturekit.FakeJJ

// unreachableGitHub is where a fixture's error reporter files unless a test
// points it at a fake: a local port nothing listens on, so no test can reach
// the real GitHub.
const unreachableGitHub = "http://127.0.0.1:1"

func testOptions(cfg config.Options) Options {
	return Options{Config: cfg, Build: Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{stream}, ShutdownTimeout: 100 * time.Millisecond,
		ErrorReporter: ErrorReporterOptions{BaseURL: unreachableGitHub}}
}

type blockingTurns = fixturekit.BlockingTurns

type completedRunner = fixturekit.CompletedRunner

var amendmentOp = intakefixture.AmendmentOp

var apiError = intakefixture.ApiError
