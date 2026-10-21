package lifecycle_test

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const demoAgent = intakefixture.DemoAgent
const demoThread = intakefixture.DemoThread

type demoWorkspaces = fixturekit.DemoWorkspaces
type demoTransport = fixturekit.DemoTransport
type demoTurn = intakefixture.DemoTurn
type chiefTurns = fixturekit.ChiefTurns

var jump = fixturekit.Jump

type fakeIssues = fixturekit.FakeIssues

var request = fixturekit.Request
var traceExec = fixturekit.TraceExec
var workstreamTraceDirectory = intakefixture.WorkstreamTraceDirectory
var projectDocument = fixturekit.ProjectDocument
var traceCommits = fixturekit.TraceCommits
var traceFile = fixturekit.TraceFile

func testOptions(cfg config.Options) Options {
	return Options{Options: service.Options{Config: cfg, Build: Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{stream}, ShutdownTimeout: 100 * time.Millisecond}}
}

type blockingTurns = fixturekit.BlockingTurns
