package lifecycle_test

import (
	"context"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newArchitectFixture(t *testing.T, configure ...func(*Options)) *architectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	for _, apply := range configure {
		apply(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, "1. Keep changes small.\n2. Every change has a test.\n", func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	f.s.cfg = f.s.current()
	return f
}

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

func (f *architectFixture) runs() []string {
	return f.scenario().Runs()
}
func (s *Service) answers(cfg *config.Config, r *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, r)
}
