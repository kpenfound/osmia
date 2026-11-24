package service

import (
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const masonRoles = intakefixture.ChiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

func newMasonFixture(t *testing.T, masons int, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureOn(t, config.WorkspacesGit, fmt.Sprintf("masons = %d\n", masons), drafted)
}

func newMasonFixtureOn(t *testing.T, backend, capacity, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, backend, capacity, drafted, nil)
}

func newMasonFixturePrepared(t *testing.T, masons int, drafted string, prepare func(*Options)) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, config.WorkspacesGit, fmt.Sprintf("masons = %d\n", masons), drafted, prepare)
}

func countingSchedule() (*atomic.Int64, func(*Options)) {
	passed, scheduled := intakefixture.ScheduleCounter()
	return passed, func(opts *Options) { opts.schedulePassed = scheduled }
}

func newMasonFixtureWith(t *testing.T, backend, capacity, drafted string, prepare func(*Options)) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, masonRoles, func(opts *Options) {
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
