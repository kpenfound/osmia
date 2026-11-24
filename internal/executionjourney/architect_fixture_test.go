package executionjourney

import (
	context "context"
	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	jevfixture "github.com/kpenfound/osmia/internal/jev/jevfixture"
	service "github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	sync "sync"
	testing "testing"
)

type architectFixture struct {
	opts        Options
	s           *Service
	serviceDone chan struct{}
	c           *Client
	project     config.ProjectID
	trace       string
	clone       string
	engine      *fixturekit.DemoEngine
	sessions    *fixturekit.DemoSessions
	clock       *fixturekit.DemoClock

	scenarioOnce  sync.Once
	scenarioState *intakefixture.ArchitectScenario
}

func newArchitectOptions(t *testing.T) (Options, string, *fixturekit.DemoEngine, *fixturekit.DemoSessions, *fixturekit.DemoClock) {
	t.Helper()
	opts, clone := projectFixture(t)
	engine, sessions, clock, architect, issues := intakefixture.ArchitectExecution(t, opts.Config, clone)
	opts.Reconciliation.Now, opts.Architect, opts.Issues = clock.Now, architect, issues
	return opts, clone, engine, sessions, clock
}

func (f *architectFixture) start(t *testing.T) {
	t.Helper()
	f.s, f.c, f.serviceDone = intakefixture.StartArchitect(t, func(ctx context.Context) (*Service, error) { return Start(ctx, f.opts) }, NewClient, func(c *Client) {
		if f.opts.WriteTimeout > c.defaultTimeout {
			c.defaultTimeout = f.opts.WriteTimeout
			c.Client.SetTimeoutForTest(c.defaultTimeout)
		}
	})

	service.InspectForTest(f.s.core()).SetRouterJudge(jevfixture.TransitionJudge(f.s.now, &jevfixture.Transitions{Free: true}))
}

func (f *architectFixture) stop(t *testing.T) {
	t.Helper()
	intakefixture.StopArchitect(t, f.c.Close, f.s.Close, f.serviceDone)
}

func (f *architectFixture) repository() *trace.Repository { return f.s.sole().repository }

func (f *architectFixture) script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.scenario().Script(turn, files, check)
}

func (f *architectFixture) scenario() *intakefixture.ArchitectScenario {
	f.scenarioOnce.Do(func() {
		f.scenarioState = &intakefixture.ArchitectScenario{Client: func() intakefixture.WorkflowClient { return f.c }, Engine: f.engine, Repository: f.repository, Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) }}
	})
	return f.scenarioState
}

func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.clone)
}

func (f *architectFixture) awaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	f.scenario().AwaitFeature(t, stream, want)
}

func (f *architectFixture) notice(t *testing.T, stream config.WorkstreamID, transition string) string {
	t.Helper()
	return intakefixture.Notice(t, f.repository(), stream, transition)
}

func (f *architectFixture) await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	f.scenario().Await(t, stream, want)
}
