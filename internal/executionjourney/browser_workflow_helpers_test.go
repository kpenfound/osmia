package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
)

var amendmentRulingTurn = intake.AmendmentRulingTurn
var checkPath = execution.CheckPath

func (f *foreman) RequestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return service.DriftForTest(f.masons.s.core(), f.masons.repository).Request(ctx, due)
}
