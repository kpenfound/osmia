package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
)

func (f *shedFixture) awaitCompleted(t *testing.T, stream config.WorkstreamID, agent, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitCompleted(t, f.repository(), stream, agent, turn)
}
