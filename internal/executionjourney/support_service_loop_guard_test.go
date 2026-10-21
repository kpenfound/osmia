package executionjourney

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"testing"

	"github.com/kpenfound/osmia/internal/config"

	"github.com/kpenfound/osmia/internal/trace"
)

func moveUnitNow(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit, to string) {
	t.Helper()
	intakefixture.MoveUnitNow(t, f.clock.Now(), foremanActor, repository, stream, unit, to)
}
