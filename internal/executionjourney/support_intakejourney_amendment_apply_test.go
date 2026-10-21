package executionjourney

import (
	context "context"

	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func landingReason(t *testing.T, lands *foreman, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	approval := &foreman{masons: &masons{repository: repository}}
	return intakefixture.LandingReason(t, stream, unit, approval.approval,
		func(result UnitReviewResult) UnitReviewIdentity { return result.Identity },
		func(ctx context.Context, stream config.WorkstreamID, in intakefixture.LandingApproval, result UnitReviewResult) (string, error) {
			return lands.current(ctx, stream, landInput{Unit: in.Unit, Review: in.Review, Candidate: in.Candidate, Base: in.Base}, result)
		})
}
