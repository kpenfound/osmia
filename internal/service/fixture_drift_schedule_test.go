package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// restartTrace closes repository and opens the project trace again, as a
// service restart does.
func restartTrace(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	return fixturekit.ReopenTrace(t, f.s.cfg, repository)
}
