package lifecycle_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type landingDemo struct {
	mu      sync.Mutex
	reviews map[string][]UnitReviewIdentity
}

func (d *landingDemo) reviewed(unit string) []UnitReviewIdentity {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reviews[unit])
}
func (d *landingDemo) review(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	return intakefixture.LandingReview(ctx, unit, req, tools, func(unit string, identity UnitReviewIdentity) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.reviews[unit] = append(d.reviews[unit], identity)
	})
}

func landingByUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) map[string]trace.Document {
	t.Helper()
	return intakefixture.LandingByUnit(t, repository, stream, landingDocument)
}
