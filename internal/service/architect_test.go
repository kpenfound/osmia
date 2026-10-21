package service

import (
	context "context"
	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	sync "sync"
	testing "testing"
)

// architectFixture is an active project with a ruled charter, a clone whose
// tracked files seed the entity map, and the fake engine the architect's
// turns run in.
type architectFixture struct {
	opts        Options
	s           *Service
	serviceDone chan struct{}
	c           *Client
	project     config.ProjectID
	trace       string
	clone       string
	engine      *demoEngine
	sessions    *demoSessions
	clock       *demoClock

	scenarioOnce  sync.Once
	scenarioState *intakefixture.ArchitectScenario
}

func newArchitectOptions(t *testing.T) (Options, string, *demoEngine, *demoSessions, *demoClock) {
	t.Helper()
	opts, clone := projectFixture(t)
	engine, sessions, clock, architect, issues := intakefixture.ArchitectExecution(t, opts.Config, clone)
	opts.Reconciliation.Now, opts.Architect, opts.Issues = clock.Now, architect, issues
	return opts, clone, engine, sessions, clock
}

func newArchitectFixture(t *testing.T, configure ...func(*Options)) *architectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	for _, apply := range configure {
		apply(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, "1. Keep changes small.\n2. Every change has a test.\n", func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	return f
}

func (f *architectFixture) start(t *testing.T) {
	t.Helper()
	f.s, f.c, f.serviceDone = intakefixture.StartArchitect(t, func(ctx context.Context) (*Service, error) { return Start(ctx, f.opts) }, NewClient, func(c *Client) {
		if f.opts.WriteTimeout > c.defaultTimeout {
			c.defaultTimeout = f.opts.WriteTimeout
		}
	})
}

func (f *architectFixture) stop(t *testing.T) {
	t.Helper()
	intakefixture.StopArchitect(t, f.c.Close, f.s.Close, f.serviceDone)
}

func (f *architectFixture) handIn(t *testing.T, key, content string) config.WorkstreamID {
	t.Helper()
	return f.scenario().HandIn(t, f.project, key, content)
}

func (f *architectFixture) repository() *trace.Repository { return f.s.sole().repository }

// script installs the fake architect's behaviour for one turn: it delivers
// the given files and returns a successful result.
func (f *architectFixture) script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.scenario().Script(turn, files, check)
}

// await polls the workstream's feature and draft states until want accepts
// them.
func (f *architectFixture) await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	f.scenario().Await(t, stream, want)
}

var sketched = intakefixture.Sketched

func (f *architectFixture) documents(t *testing.T, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	return f.scenario().Documents(t, stream, id)
}

func (f *architectFixture) draftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().DraftOperations(t, stream)
}

func (f *architectFixture) architectThread(t *testing.T, stream config.WorkstreamID) trace.Thread {
	t.Helper()
	return f.scenario().ArchitectThread(t, stream)
}

func (f *architectFixture) scenario() *intakefixture.ArchitectScenario {
	f.scenarioOnce.Do(func() {
		f.scenarioState = &intakefixture.ArchitectScenario{Client: func() intakefixture.WorkflowClient { return f.c }, Engine: f.engine, Repository: f.repository, Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) }}
	})
	return f.scenarioState
}

func (f *architectFixture) sharedTurns() *intakefixture.TurnScenario {
	return &intakefixture.TurnScenario{
		Root: func() config.Root { return f.s.current().Root }, Project: f.project,
		Now: f.clock.Now, Repository: f.repository,
		Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) },
		Done:    func() <-chan struct{} { return f.s.done }, WaitError: f.s.Wait, ForemanActor: foremanActor,
	}
}
