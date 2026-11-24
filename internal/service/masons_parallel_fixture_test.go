package service

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const disjointPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "task": "Send chunks", "acceptance": ["spec#1 holds, shown by TestUpload (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

func newParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureOn(t, backend, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted)
	intakefixture.ExtendParallelMasons(t, f.clone, f.repository(), f.clock.Now(), f.engine, fake, masonTurnID, ownerActor)
	return f, fake
}

func newParallelMasonFixtureCounting(t *testing.T, masons, perWorkstream int, drafted string) (*shedFixture, *intakefixture.FakeMasons, *atomic.Int64) {
	t.Helper()
	passed, prepare := countingSchedule()
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted, prepare)
	intakefixture.ExtendParallelMasons(t, f.clone, f.repository(), f.clock.Now(), f.engine, fake, masonTurnID, ownerActor)
	return f, fake, passed
}

func starts(t *testing.T, f *shedFixture, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.MasonStarts(intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream))
}

func unitStatus(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) UnitStatus {
	t.Helper()
	return intakefixture.ReadUnitStatus(t, f.c, stream, unit)
}

func (f *shedFixture) deferred(t *testing.T, stream config.WorkstreamID, unit string, d UnitDispatch) UnitStatus {
	t.Helper()
	state, _ := f.s.store.Effective()
	return intakefixture.DeferredUnit(t, f.repository(), state.Pauses, stream, unit, d)
}

func newParallelMasonFixture(t *testing.T, masons, perWorkstream int, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return newParallelMasonFixtureOn(t, config.WorkspacesGit, masons, perWorkstream, drafted)
}

func (f *shedFixture) dispatches(t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	return intakefixture.DispatchDecisions(t, f.repository(), stream, unit)
}

func (f *shedFixture) awaitDispatches(t *testing.T, stream config.WorkstreamID, unit string, n int) {
	t.Helper()
	intakefixture.AwaitDispatchDecisions(t, f.repository(), stream, unit, n)
}
