package service

import (
	context "context"

	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
