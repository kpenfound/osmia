package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// driftRecords returns every recorded revision of drift/rebase.json.
func driftRecords(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []DriftRebase {
	t.Helper()
	return intakefixture.DriftRecords[DriftRebase](t, repository, stream, driftDocument)
}

var seals = intakefixture.Seals

// testDrift makes every eligible workstream due for a drift rebase.
func testDrift(config.WorkstreamID) (string, error) { return "the test asks for a drift rebase", nil }

// requestDrift asks for drift rebases and returns the one operation it
// asked for, for the workstream.
func requestDrift(t *testing.T, d drifter, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	return intakefixture.RequestDrift(t, d.repository, stream, DriftAction, d.RequestDrifts, testDrift)
}

// settleOperation reconciles one pending operation as the reconciliation
// controller does: inspection first, the effect only when it is absent, and
// the result recorded on the operation, at the time of the service's clock.
func settleOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	return intakefixture.SettleOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

// parentOf returns the first parent of a commit of the fixture's clone.
func parentOf(t *testing.T, f *shedFixture, commit string) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", commit+"^"))
}

// fileAt returns a file of a commit of the fixture's clone.
func fileAt(t *testing.T, f *shedFixture, commit, path string) string {
	t.Helper()
	return demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "show", commit+":"+path)
}
