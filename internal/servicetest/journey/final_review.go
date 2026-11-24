package journey

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func HostService(t *testing.T, f *ShedFixture, stream func() config.WorkstreamID) (string, *fixturekit.FakePulls) {
	t.Helper()
	var fork string
	var pulls *fixturekit.FakePulls
	fork, pulls = fixturekit.PublicationFork(t, f.Clone, func() string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, intake.FeatureBranch(stream()))
		return tip
	})
	pulls.Branches = func(branch string) string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, branch)
		return tip
	}
	pulls.SyncHead = true
	f.S.Options.PullRequests = pulls
	f.Opts.PullRequests = pulls
	return fork, pulls
}
