package lifecycle_test

import (
	"context"
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

// twoProjectFixture is a root with two active projects that share one mason
// slot, each with its own trace, workstream and mason thread.
type twoProjectFixture struct {
	opts     Options
	cfg      *config.Config
	clock    *demoClock
	turns    *blockingTurns
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

var turn = intakefixture.Turn

// block makes the named turn signal entered and wait for release or for its
// context to end.
func (f *twoProjectFixture) block(id config.ProjectID, name string) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	f.turns.Mu.Lock()
	f.turns.Hooks[turn(id, name)] = func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	f.turns.Mu.Unlock()
	return entered, release
}
func (f *twoProjectFixture) repository(id config.ProjectID) *trace.Repository {
	return f.scenario.Repository(id)
}

var await = intakefixture.Await
