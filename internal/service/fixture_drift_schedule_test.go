package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func restartTrace(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	return fixturekit.ReopenTrace(t, f.s.cfg, repository)
}
