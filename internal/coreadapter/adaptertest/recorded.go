package adaptertest

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/kpenfound/agentcore/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
)

// RecordingHosts is an MCPHosts that records each request and hosts nothing:
// the endpoint it returns is never reached, so it stands in for the real
// server only where a test reads what the service offered a turn.
type RecordingHosts struct {
	mu       sync.Mutex
	Requests []a.HostRequest
}

func (h *RecordingHosts) Host(_ context.Context, req a.HostRequest) (a.HostedMCP, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Requests = append(h.Requests, req)
	return a.HostedMCP{Endpoint: a.Endpoint{URL: "http://service/mcp", BearerTokenEnvironment: a.TokenEnvironment, Token: "recorded-token"}, Lease: releaseFunc(func(context.Context) error { return nil })}, nil
}

type releaseFunc func(context.Context) error

func (f releaseFunc) Release(ctx context.Context) error { return f(ctx) }

// StubTools returns a read tool for each name that does nothing, so a role's
// granted tool list can be served without the service state its real handlers
// need.
func StubTools(names ...string) []a.Tool {
	tools := make([]a.Tool, len(names))
	for i, name := range names {
		tools[i] = a.Tool{Name: name, Effect: a.ToolRead, InputSchema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }}
	}
	return tools
}

// Turn is what one turn run through the service's isolation reached the
// engine and the MCP host with.
type Turn struct {
	// Native is the tool list the engine granted the turn: its own tools,
	// followed by the entry of the service's MCP server. MCP names the service
	// tools the host was asked to serve.
	Native       []string
	MCP          []string
	SystemPrompt string
	Mounts       []agent.Mount
	VCS          bool
	// Capabilities are the permissions the host was told the turn holds.
	Capabilities a.Capabilities
}

// LastTurn reports the most recent turn that ran on the engine, with what the
// host was last asked to serve; a turn with no service tool never reaches the
// host, so a test reads one turn per engine and host. It reports false when no
// turn ran.
func LastTurn(engine *Engine, hosts *RecordingHosts) (Turn, bool) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	hosts.mu.Lock()
	defer hosts.mu.Unlock()
	if len(engine.Requests) == 0 {
		return Turn{}, false
	}
	req := engine.Requests[len(engine.Requests)-1]
	turn := Turn{SystemPrompt: req.SystemPrompt}
	if req.Grants != nil {
		turn.Native, turn.Mounts, turn.VCS = slices.Clone(req.Grants.Tools), slices.Clone(req.Grants.Mounts), req.Grants.VCS
	}
	if len(hosts.Requests) > 0 {
		hosted := hosts.Requests[len(hosts.Requests)-1]
		turn.Capabilities = hosted.Capabilities
		for _, tool := range hosted.Tools {
			turn.MCP = append(turn.MCP, tool.Name)
		}
	}
	return turn, true
}
