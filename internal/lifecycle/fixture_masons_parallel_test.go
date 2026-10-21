package lifecycle_test

import (
	"fmt"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const disjointPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

func newParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureOn(t, backend, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted, "")
	intakefixture.ExtendParallelMasons(t, f.repository(), f.clock.Now(), f.engine, fake, masonTurnID, librarianActor)
	return f, fake
}
