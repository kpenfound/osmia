package executionjourney

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func checkNoRetry(t *testing.T, op trace.OperationRecord, outcome string) {
	t.Helper()
	intakefixture.CheckNoRetry(t, op, outcome)
}

func checkPauseStop(t *testing.T, q trace.QueuedTurn, scope, reason string) {
	t.Helper()
	intakefixture.CheckPauseStop(t, q, scope, reason)
}
