package executionjourney

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func awaitRefresh(t *testing.T, f *shedFixture) trace.OperationRecord {
	t.Helper()
	return intakefixture.AwaitRefresh(t, f.repository(), f.project, RefreshAction, librarianAgent)
}
