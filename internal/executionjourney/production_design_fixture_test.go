package executionjourney

import (
	context "context"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

func seedBuilding(f *journey.ShedFixture, t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	return intakefixture.SeedBuilding(t, f.Clone, key, graph, func() (string, error) {
		b, err := newWorkspaces(f.S, context.Background(), f.S.About(f.Repository()))
		return b.Backend, err
	}, func(k, g, b, base string) config.WorkstreamID {
		return journey.RecordBuild(t, f, f.Repository(), k, g, b, base)
	}, f.S.RefreshRuntime)
}
