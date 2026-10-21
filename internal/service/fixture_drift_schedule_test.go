package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// restartTrace closes repository and opens the project trace again, as a
// service restart does.
func restartTrace(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	must(t, repository.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	return reopened
}
