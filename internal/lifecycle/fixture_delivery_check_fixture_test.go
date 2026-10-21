package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *shedFixture) buildFixture() intakefixture.BuildFixture {
	return intakefixture.BuildFixture{
		Upstream: f.upstream,
		Seed: func(t *testing.T, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
			return seedBuild(t, f, key, graph, backend, base)
		},
		Start: f.start,
	}
}
