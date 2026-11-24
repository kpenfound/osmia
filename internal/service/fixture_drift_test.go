package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/lifecycle"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func driftRecords(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []DriftRebase {
	t.Helper()
	return intakefixture.DriftRecords[DriftRebase](t, repository, stream, lifecycle.DriftDocument)
}

func requestDrift(t *testing.T, d drifter, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	return intakefixture.RequestDrift(t, d.repository, stream, DriftAction, d.RequestDrifts, intakefixture.TestDrift)
}

func settleOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	return intakefixture.SettleOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

func parentOf(t *testing.T, f *shedFixture, commit string) string {
	t.Helper()
	return intakefixture.ParentOf(t, f.clone, commit)
}

func fileAt(t *testing.T, f *shedFixture, commit, path string) string {
	t.Helper()
	return intakefixture.FileAt(t, f.clone, commit, path)
}
