package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func seedAmendment(t *testing.T, f *architectFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.SeedAmendment(t, f.project, f.clock.Now, repo, stream)
}
