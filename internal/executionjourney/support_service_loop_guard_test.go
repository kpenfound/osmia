package executionjourney

import (
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func moveUnitNow(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit, to string) {
	t.Helper()
	intakefixture.MoveUnitNow(t, f.clock.Now(), foremanActor, repository, stream, unit, to)
}
