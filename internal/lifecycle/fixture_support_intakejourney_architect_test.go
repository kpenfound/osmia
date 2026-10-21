package lifecycle_test

import (
	"context"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var readTool = intakefixture.ReadTool

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
