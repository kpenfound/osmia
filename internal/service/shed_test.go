package service

import (
	"context"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const shedCharter = intakefixture.ShedCharter

// shedFixture is an architect fixture whose service also runs the committee,
// in a container, with the given number of members.
type shedFixture struct {
	*architectFixture
	members int
}

// newDebateFixture debates for at most the given number of rounds. The
// architect's replies are silent unless a test scripts them.
func newDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	t.Helper()
	return newDebateFixtureWith(t, members, rounds, "", nil)
}

// newSketchedDebateFixture leaves the committee out of the initial service so
// setup can stop at sketched before planting an amendment. stopAtSketch
// restores the runner for manual passes and subsequent service starts.
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
	f.await(t, stream, sketched)
	f.stop(t)
	f.s.options.Committee = f.opts.Committee
}

// newDebateFixtureWith is newDebateFixture with extra configuration appended
// and prepare, when given, adjusting the options before the service starts.
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
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, shedCharter, func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	f.scenario().ScriptDebate(rounds)
	return &shedFixture{architectFixture: f, members: members}
}

// member installs the fake behaviour of one member's turn. Errors it returns
// fail the turn, so checks collect theirs for the test to read.
func (f *shedFixture) member(round, i, attempt int, run func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) string {
	return f.scenario().Member(round, i, attempt, run)
}

// awaitShed waits until the workstream's shed has reached the wanted state,
// whatever it moved to since. It fails at once when the shed reaches a state
// in never, after which the wanted one cannot come: by default, for a heard
// round, its failure, and for a conclusion, the next round.
func (f *shedFixture) awaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	f.scenario().AwaitShed(t, stream, want, never...)
}
