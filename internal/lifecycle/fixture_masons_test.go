package lifecycle_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const masonRoles = chiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

type fakeMasons = intakefixture.FakeMasons

func newMasonFixture(t *testing.T, masons int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newCappedMasonFixture(t, fmt.Sprintf("masons = %d\n", masons), drafted)
}
func newCappedMasonFixture(t *testing.T, capacity, drafted string) (*shedFixture, *fakeMasons) {
	return newConfiguredMasonFixture(t, capacity, drafted, "")
}
func newConfiguredMasonFixture(t *testing.T, capacity, drafted, classifier string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newMasonFixtureOn(t, config.WorkspacesGit, capacity, drafted, classifier)
}
func newMasonFixtureOn(t *testing.T, backend, capacity, drafted, classifier string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, backend, capacity, drafted, classifier, nil)
}
func newMasonFixtureWith(t *testing.T, backend, capacity, drafted, classifier string, prepare func(*Options)) (*shedFixture, *fakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, masonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, backend, capacity, classifier, opts.Threads)
		if prepare != nil {
			prepare(opts)
		}
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: drafted}, nil)
	f.upstream(t)
	return f, intakefixture.ScriptMasons(f.engine, masonTurnID)
}

var awaitPasses = intakefixture.AwaitPasses

func (f *shedFixture) awaitEventTurns(t *testing.T, stream config.WorkstreamID, want ...string) {
	t.Helper()
	intakefixture.AwaitEventTurns(t, f.repository(), stream, want...)
}
