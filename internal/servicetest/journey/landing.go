package journey

import (
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func ApprovedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, service.UnitReviewResult) {
	t.Helper()
	f := &Foreman{Masons: &Masons{Repository: repository}}
	return intakefixture.ApprovedReview(t, stream, unit, f.Approval)
}

func (f *ShedFixture) AwaitMerged(t *testing.T, stream config.WorkstreamID, unit string) {
	t.Helper()
	intakefixture.AwaitMerged(t, f.SharedTurns(), stream, unit, func(t *testing.T) []trace.OperationRecord { return LandOperations(t, f.Repository(), stream) })
}
