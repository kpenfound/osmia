package fixturekit

import (
	"context"

	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type FakeTurn = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error
