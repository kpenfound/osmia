package service

import (
	"context"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type shedFixture struct {
	*architectFixture
	members int
}

func newDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	t.Helper()
	return newDebateFixtureWith(t, members, rounds, "", nil)
}

func newSketchedDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	t.Helper()
	var committee *Committee
	f := newDebateFixtureWith(t, members, rounds, "", func(o *Options) {
		committee, o.Committee = o.Committee, nil
	})
	f.opts.Committee = committee
	return f
}

func (f *shedFixture) stopAtSketch(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	f.await(t, stream, intakefixture.Sketched)
	f.stop(t)
	f.s.options.Committee = f.opts.Committee
}

func newDebateFixtureWith(t *testing.T, members, rounds int, extra string, prepare func(*Options)) *shedFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	intakefixture.ConfigureDebate(t, opts.Config.Root, members, rounds, extra)
	opts.Committee = &Committee{Engine: engine, Hosts: opts.Architect.Hosts}
	if prepare != nil {
		prepare(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, intakefixture.ShedCharter, func(t *testing.T, p config.ProjectID, text string) { intakefixture.WriteCharter(t, f.c, p, text) })
	f.scenario().ScriptDebate(rounds)
	return &shedFixture{architectFixture: f, members: members}
}

func (f *shedFixture) member(round, i, attempt int, run func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) string {
	return f.scenario().Member(round, i, attempt, run)
}

func (f *shedFixture) awaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	f.scenario().AwaitShed(t, stream, want, never...)
}
