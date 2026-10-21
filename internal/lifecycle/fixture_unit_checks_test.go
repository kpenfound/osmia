package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func runChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &checkers{masons: newMasonController(s, repository)}, CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, repository, stream, op, r)
	})
}
