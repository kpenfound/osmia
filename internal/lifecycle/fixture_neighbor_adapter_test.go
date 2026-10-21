package lifecycle_test

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

func (d drifter) driver() *service.TestDriftDriver {
	return service.DriftForTest(d.s.core(), d.repository)
}
func (d drifter) requestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return d.driver().Request(ctx, due)
}

type refreshInput = service.TestRefreshInput

const RefreshAction = service.RefreshAction
const driftMasonAgent = service.TestDriftMasonAgent
const driftReviewerAgent = service.TestDriftReviewerAgent
const driftsDirectory = service.TestDriftsDirectory
