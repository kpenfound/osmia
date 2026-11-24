package lifecycle_test

import (
	"fmt"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const masonRoles = intakefixture.ChiefRole + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.reviewer]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

func newMasonFixture(t *testing.T, masons int, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newCappedMasonFixture(t, fmt.Sprintf("masons = %d\n", masons), drafted)
}
func newCappedMasonFixture(t *testing.T, capacity, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	return newConfiguredMasonFixture(t, capacity, drafted)
}
func newConfiguredMasonFixture(t *testing.T, capacity, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureOn(t, config.WorkspacesGit, capacity, drafted)
}
func newMasonFixtureOn(t *testing.T, backend, capacity, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newMasonFixtureWith(t, backend, capacity, drafted, nil)
}
func newMasonFixtureWith(t *testing.T, backend, capacity, drafted string, prepare func(*Options)) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, masonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, service.Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, backend, capacity, opts.Threads)
		if prepare != nil {
			prepare(opts)
		}
	})
	f.script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: drafted}, nil)
	f.upstream(t)
	return f, intakefixture.ScriptMasons(f.engine, execution.MasonTurnID)
}

func (f *shedFixture) awaitEventTurns(t *testing.T, stream config.WorkstreamID, want ...string) {
	t.Helper()
	intakefixture.AwaitEventTurns(t, f.repository(), stream, want...)
}

const independentPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.scenario().BuiltAs(t, key)
}

const disjointPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "task": "Send chunks", "acceptance": ["spec#1 holds, shown by TestUpload (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

func newParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureOn(t, backend, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted)
	intakefixture.ExtendParallelMasons(t, f.clone, f.repository(), f.clock.Now(), f.engine, fake, execution.MasonTurnID, intake.OwnerActor)
	return f, fake
}
