package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func newShedFixture(t *testing.T, members int) *shedFixture {
	t.Helper()
	return newDebateFixture(t, members, 1)
}

func newDebateFixtureWith(t *testing.T, members, rounds int, extra string, prepare func(*Options)) *shedFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	intakefixture.ConfigureDebate(t, opts.Config.Root, members, rounds, extra)
	opts.Committee = &intake.Committee{Engine: engine, Hosts: opts.Architect.Hosts}
	if prepare != nil {
		prepare(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, intakefixture.ShedCharter, func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	f.s.cfg = f.s.current()
	f.scenario().ScriptDebate(rounds)
	return &shedFixture{architectFixture: f, members: members}
}

type shedFixture struct {
	*architectFixture
	members int
}

func newDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	return newDebateFixtureWith(t, members, rounds, "", nil)
}

func (f *shedFixture) thread(t *testing.T, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	return intakefixture.Thread(t, f.repository(), stream, member)
}

func (f *shedFixture) unitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}
