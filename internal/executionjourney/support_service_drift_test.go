package executionjourney

import (
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	filepath "path/filepath"
	strings "strings"
	testing "testing"
)

func driftRecords(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []DriftRebase {
	t.Helper()
	return intakefixture.DriftRecords[DriftRebase](t, repository, stream, driftDocument)
}

func fileAt(t *testing.T, f *shedFixture, commit, path string) string {
	t.Helper()
	return demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "show", commit+":"+path)
}

func parentOf(t *testing.T, f *shedFixture, commit string) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", commit+"^"))
}

var seals = intakefixture.Seals

func settleOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	return intakefixture.SettleOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

func requestDrift(t *testing.T, d drifter, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	return intakefixture.RequestDrift(t, d.repository, stream, DriftAction, d.requestDrifts, testDrift)
}

func testDrift(config.WorkstreamID) (string, error) { return "the test asks for a drift rebase", nil }
