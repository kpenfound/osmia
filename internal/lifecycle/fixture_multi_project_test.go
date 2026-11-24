package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	otherProject config.ProjectID    = "p_fedcba9876543210fedcba9876543210"
	otherStream  config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"
)

type twoProjectFixture struct {
	opts     Options
	cfg      *config.Config
	clock    *fixturekit.DemoClock
	turns    *fixturekit.BlockingTurns
	streams  map[config.ProjectID]config.WorkstreamID
	scenario *fixturekit.TwoProjectScenario
}

func newTwoProjectFixture(t *testing.T) *twoProjectFixture {
	t.Helper()
	data := fixturekit.NewTwoProjectScenario(t)
	opts := testOptions(data.Config)
	opts.Workstreams, opts.Threads, opts.Reconciliation.Now = nil, data.Threads, data.Clock.Now
	return &twoProjectFixture{opts: opts, cfg: data.Loaded, clock: data.Clock, streams: data.Streams, turns: data.Turns, scenario: data}
}

func (f *twoProjectFixture) block(id config.ProjectID, name string) (entered, release chan struct{}) {
	return f.turns.Block(intakefixture.Turn(id, name))
}
func (f *twoProjectFixture) repository(id config.ProjectID) *trace.Repository {
	return f.scenario.Repository(id)
}
