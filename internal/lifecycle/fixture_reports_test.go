package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var resumeReport = "resume from the last chunk"
var exampleCard = intakefixture.ExampleCard

func newMasonController(s *Service, repository *trace.Repository) *masons {
	return &masons{s: s, cfg: s.cfg, repository: repository}
}

var done = intakefixture.Done

func reportDone(outcome string) func(context.Context, agent.Request, *mcp.ClientSession) error {
	return intakefixture.ReportDone(outcome)
}
func (f *shedFixture) awaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	f.sharedTurns().AwaitUnit(t, stream, unit, state)
}
