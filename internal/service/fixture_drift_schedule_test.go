package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func restartTrace(t *testing.T, f *shedFixture, repository *trace.Repository) *trace.Repository {
	t.Helper()
	return fixturekit.ReopenTrace(t, f.s.cfg, repository)
}
