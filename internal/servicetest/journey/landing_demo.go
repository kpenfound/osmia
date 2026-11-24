package journey

import (
	context "context"
	slices "slices"
	sync "sync"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type LandingDemo struct {
	Mu      sync.Mutex
	Reviews map[string][]service.UnitReviewIdentity
}

func (d *LandingDemo) Reviewed(unit string) []service.UnitReviewIdentity {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	return slices.Clone(d.Reviews[unit])
}

func (d *LandingDemo) Review(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	return intakefixture.LandingReview(ctx, unit, req, tools, func(unit string, identity service.UnitReviewIdentity) {
		d.Mu.Lock()
		defer d.Mu.Unlock()
		d.Reviews[unit] = append(d.Reviews[unit], identity)
	})
}

func LandingByUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) map[string]trace.Document {
	t.Helper()
	return intakefixture.LandingByUnit(t, repository, stream, execution.LandingDocument)
}
