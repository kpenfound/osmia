package service

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"os"
	"sync"
	"testing"
	"time"
)

type crossRepoWiringFixture struct {
	opts               Options
	cfg                *config.Config
	clock              *fixedClock
	engine             *demoEngine
	ticks              chan time.Time
	project1, project2 config.ProjectID
	stream1, stream2   config.WorkstreamID
	mu                 sync.Mutex
	live               map[config.ProjectID]*trace.Repository
	problems           []string
}

func newCrossRepoWiringFixture(t *testing.T, agents map[string]string) *crossRepoWiringFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "wgw-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	opts.Workstreams = nil
	f := &crossRepoWiringFixture{opts: opts, project1: project, project2: otherProject, stream1: stream, stream2: otherStream, live: map[config.ProjectID]*trace.Repository{}}
	setup := intakefixture.NewWiringSetup(t, home, opts.Config, agents, func(r *trace.Repository) {
		f.mu.Lock()
		f.live[r.Project()] = r
		f.mu.Unlock()
	})
	f.cfg, f.clock, f.engine, f.ticks = setup.Config, setup.Clock, setup.Engine, setup.Ticks
	f.opts.Threads = setup.Threads
	f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = setup.Clock.Now, setup.Ticks
	return f
}
