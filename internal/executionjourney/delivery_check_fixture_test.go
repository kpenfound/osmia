package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func finalCheckStarts(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []FinalCheckAttempt {
	t.Helper()
	return fixturekit.DocumentValues(t, repo, stream, func(doc trace.Document, start FinalCheckAttempt) bool {
		return start.Attempt > 0 && doc.ID == finalCheckAttemptDocument(start.Review, start.Attempt)
	})
}

func (f *shedFixture) buildFixture() intakefixture.BuildFixture {
	return intakefixture.BuildFixture{
		Upstream: f.upstream,
		Seed: func(t *testing.T, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
			return seedBuild(t, f, key, graph, backend, base)
		},
		Start: f.start,
	}
}

func sealDependent(t *testing.T, f *shedFixture, repo *trace.Repository, backend string, base config.WorkstreamID, files map[string]string) (config.WorkstreamID, string) {
	t.Helper()
	return intakefixture.SealDependent(t, f.s.intake(), repo, backend, base, files)
}
