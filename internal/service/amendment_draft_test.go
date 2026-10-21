package service

import (
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func seedAmendment(t *testing.T, f *architectFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.SeedAmendment(t, f.project, f.clock.Now, repo, stream)
}

var readDocs = intakefixture.ReadDocs
