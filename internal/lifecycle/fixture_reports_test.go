package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var resumeReport = "resume from the last chunk"

func newMasonController(s *Service, repository *trace.Repository) *masons {
	return &masons{s: s, cfg: s.cfg, repository: repository}
}

func (f *shedFixture) awaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	f.sharedTurns().AwaitUnit(t, stream, unit, state)
}
