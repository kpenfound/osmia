package servicetest

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type TwoProjectFixture struct {
	Options  service.Options
	Config   *config.Config
	Clock    *fixturekit.DemoClock
	Streams  map[config.ProjectID]config.WorkstreamID
	Turns    *fixturekit.BlockingTurns
	scenario *fixturekit.TwoProjectScenario
}

func NewTwoProjectFixture(t *testing.T) *TwoProjectFixture {
	t.Helper()
	data := fixturekit.NewTwoProjectScenario(t)
	opts := Options(data.Config)
	opts.Workstreams, opts.Threads, opts.Reconciliation.Now = nil, data.Threads, data.Clock.Now
	return &TwoProjectFixture{Options: opts, Config: data.Loaded, Clock: data.Clock, Streams: data.Streams, Turns: data.Turns, scenario: data}
}

func (f *TwoProjectFixture) Repository(id config.ProjectID) *trace.Repository {
	return f.scenario.Repository(id)
}
