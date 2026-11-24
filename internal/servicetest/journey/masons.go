package journey

import (
	"sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func NewMasonFixture(t *testing.T, masons int, drafted string) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return NewCappedMasonFixture(t, intakefixture.MasonCapacity(masons), drafted)
}

func NewCappedMasonFixture(t *testing.T, capacity, drafted string) (*ShedFixture, *intakefixture.FakeMasons) {
	return NewConfiguredMasonFixture(t, capacity, drafted)
}

func NewConfiguredMasonFixture(t *testing.T, capacity, drafted string) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return NewMasonFixtureOn(t, config.WorkspacesGit, capacity, drafted)
}

func (f *ShedFixture) AwaitEventTurns(t *testing.T, stream config.WorkstreamID, want ...string) {
	t.Helper()
	intakefixture.AwaitEventTurns(t, f.Repository(), stream, want...)
}

// NewMasonFixturePrepared is NewMasonFixture whose options prepare changes
// before the service starts.
func NewMasonFixturePrepared(t *testing.T, masons int, drafted string, prepare func(*Options)) (*ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	return NewMasonFixtureWith(t, config.WorkspacesGit, intakefixture.MasonCapacity(masons), drafted, prepare)
}

// NewParallelMasonFixtureCounting is NewParallelMasonFixture that also counts
// scheduling passes.
func NewParallelMasonFixtureCounting(t *testing.T, masons, perWorkstream int, drafted string) (*ShedFixture, *intakefixture.FakeMasons, *atomic.Int64) {
	t.Helper()
	passed, prepare := CountingSchedule()
	f, fake := NewParallelMasonFixturePrepared(t, config.WorkspacesGit, masons, perWorkstream, drafted, prepare)
	return f, fake, passed
}

// Starts lists the mason starts recorded for the workstream.
func Starts(t *testing.T, f *ShedFixture, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.MasonStarts(intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream))
}

// UnitStatus reads the unit's status from the service's API.
func UnitStatus(t *testing.T, f *ShedFixture, stream config.WorkstreamID, unit string) api.UnitStatus {
	t.Helper()
	return intakefixture.ReadUnitStatus(t, f.C, stream, unit)
}
