package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
)

func awaitRefresh(t *testing.T, f *shedFixture) trace.OperationRecord {
	t.Helper()
	return intakefixture.AwaitRefresh(t, f.repository(), f.project, RefreshAction, librarianAgent)
}
