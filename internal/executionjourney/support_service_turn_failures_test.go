package executionjourney

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func (f *shedFixture) awaitCompleted(t *testing.T, stream config.WorkstreamID, agent, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitCompleted(t, f.repository(), stream, agent, turn)
}
