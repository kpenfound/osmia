package journey

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type TwoProjectFixture struct {
	Opts     Options
	Cfg      *config.Config
	Clock    *fixturekit.DemoClock
	Turns    *fixturekit.BlockingTurns
	Streams  map[config.ProjectID]config.WorkstreamID
	Scenario *fixturekit.TwoProjectScenario
}

func NewTwoProjectFixture(t *testing.T) *TwoProjectFixture {
	t.Helper()
	data := fixturekit.NewTwoProjectScenario(t)
	opts := TestOptions(data.Config)
	opts.Workstreams, opts.Threads, opts.Reconciliation.Now = nil, data.Threads, data.Clock.Now
	return &TwoProjectFixture{Opts: opts, Cfg: data.Loaded, Clock: data.Clock, Streams: data.Streams, Turns: data.Turns, Scenario: data}
}

func (f *TwoProjectFixture) Repository(id config.ProjectID) *trace.Repository {
	return f.Scenario.Repository(id)
}
