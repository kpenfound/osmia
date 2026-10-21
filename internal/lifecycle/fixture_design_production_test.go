package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// newProductionDesignFixture prepares repos and injected engines only. The
// owner endpoint and production architect tools create all feature members,
// handed inputs and draft documents after the real design exchange.
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

var equalJSONTest = intakefixture.EqualJSONTest

func (f *productionDesignFixture) assertBarrier(t *testing.T) {
	t.Helper()
	f.sharedDesign().AssertBarrier(t)
}
func (f *productionDesignFixture) awaitApproved(t *testing.T) {
	t.Helper()
	f.sharedDesign().AwaitApproved(t)
}
