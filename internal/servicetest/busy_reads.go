package servicetest

import (
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// BusyReadsService starts a real service with the supplied repository adapter.
func BusyReadsService(t *testing.T, blocking coreadapter.Reconciler) (*service.Service, *service.Client, *trace.Repository) {
	t.Helper()
	opts := Options(fixturekit.BusyReadsConfig(t))
	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := Start(t, opts)
	repo, err := service.InspectForTest(s).Repository(fixturekit.Project)
	fixturekit.Must(t, err)
	if repo == nil {
		t.Fatal("no active project")
	}
	return s, c, repo
}
