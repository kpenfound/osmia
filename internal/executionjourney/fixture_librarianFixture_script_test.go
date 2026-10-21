package executionjourney

import (
	context "context"
	agent "github.com/kpenfound/agentcore/agent"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (f *librarianFixture) script(turn string, files map[string]string, check func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error) {
	fixturekit.ScriptLibrarian(f.engine, turn, files, check)
}
