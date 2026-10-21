package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
)

func checkNoRetry(t *testing.T, op trace.OperationRecord, outcome string) {
	t.Helper()
	intakefixture.CheckNoRetry(t, op, outcome)
}

func checkPauseStop(t *testing.T, q trace.QueuedTurn, scope, reason string) {
	t.Helper()
	intakefixture.CheckPauseStop(t, q, scope, reason)
}
