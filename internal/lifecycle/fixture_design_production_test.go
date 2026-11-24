package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func newProductionDesignFixture(t *testing.T) *productionDesignFixture {
	t.Helper()
	return newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
}
func (f *productionDesignFixture) wait(t *testing.T, phase string, done func() bool) {
	t.Helper()
	f.sharedDesign().Wait(t, phase, done)
}
func (f *productionDesignFixture) timeout(t *testing.T, phase string) {
	t.Helper()
	f.sharedDesign().Timeout(t, phase)
}
func (f *productionDesignFixture) logTurns(t *testing.T, repo *trace.Repository, member config.WorkstreamID) {
	t.Helper()
	f.sharedDesign().LogTurns(t, repo, member)
}
func (f *productionDesignFixture) checkContinuation(t *testing.T) {
	t.Helper()
	f.sharedDesign().CheckContinuation(t)
}

func (f *productionDesignFixture) assertBarrier(t *testing.T) {
	t.Helper()
	f.sharedDesign().AssertBarrier(t)
}
func (f *productionDesignFixture) awaitApproved(t *testing.T) {
	t.Helper()
	f.sharedDesign().AwaitApproved(t)
}

func (f *productionDesignFixture) start(t *testing.T) {
	t.Helper()
	f.architectFixture.start(t)
	f.checkRepos(t)
}
func (f *productionDesignFixture) checkRepos(t *testing.T) {
	t.Helper()
	f.sharedDesign().CheckRepos(t)
}
