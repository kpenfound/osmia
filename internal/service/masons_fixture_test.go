package service

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func newMasonFixture(t *testing.T, masons int, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureOn(t, config.WorkspacesGit, intakefixture.MasonCapacity(masons), drafted)
}

func newMasonFixtureOn(t *testing.T, backend, capacity, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, backend, capacity, drafted, nil)
}

func newMasonFixturePrepared(t *testing.T, masons int, drafted string, prepare func(*Options)) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, config.WorkspacesGit, intakefixture.MasonCapacity(masons), drafted, prepare)
}

func countingSchedule() (*atomic.Int64, func(*Options)) {
	passed, scheduled := intakefixture.ScheduleCounter()
	return passed, func(opts *Options) { opts.schedulePassed = scheduled }
}

func newMasonFixtureWith(t *testing.T, backend, capacity, drafted string, prepare func(*Options)) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, intakefixture.MasonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, backend, capacity, opts.Threads)
		if prepare != nil {
			prepare(opts)
		}
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: drafted}, nil)
	f.upstream(t)
	return f, intakefixture.ScriptMasons(f.engine, masonTurnID)
}
