package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

func (f *shedFixture) awaitMasonSettled(t *testing.T, stream config.WorkstreamID, unit string, turns int) {
	t.Helper()
	intakefixture.AwaitMasonSettled(t, f.repository(), stream, unit, turns)
}
