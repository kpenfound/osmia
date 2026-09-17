package coreadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTransport serves a server to one turn and returns where the turn reaches
// it, with the bearer token the endpoint requires. Start owns cleanup if
// startup fails; a successful lease closes the transport.
type MCPTransport interface {
	Start(context.Context, *mcp.Server) (Endpoint, Lease, error)
}
type MCPHost struct{ Transport MCPTransport }

var _ MCPHosts = (*MCPHost)(nil)

func (h *MCPHost) Host(ctx context.Context, req HostRequest) (HostedMCP, error) {
	if err := ctx.Err(); err != nil {
		return HostedMCP{}, err
	}
	server, err := scopedServer(req)
	if err != nil {
		return HostedMCP{}, err
	}
	if h.Transport == nil {
		return HostedMCP{}, unsupported("MCP transport", "no transport supplied")
	}
	endpoint, lease, err := h.Transport.Start(ctx, server)
	if err != nil {
		return HostedMCP{}, err
	}
	return HostedMCP{Endpoint: endpoint, Lease: lease}, nil
}

func scopedServer(req HostRequest) (server *mcp.Server, err error) {
	if req.Scope.Role == "" {
		return nil, errors.New("MCP host requires a role")
	}
	// The typed core registry panics for invalid schemas; request data errors are
	// returned to the service before any transport starts.
	defer func() {
		if p := recover(); p != nil {
			server = nil
			err = fmt.Errorf("invalid MCP tool: %v", p)
		}
	}()
	registry := mcphost.NewRegistry([]string{req.Scope.Role}, mcphost.RejectRole, mcphost.RejectRole)
	seen := map[string]bool{}
	for _, tool := range req.Tools {
		if !slices.Contains(req.Capabilities.Tools, tool.Name) {
			continue
		}
		if !ToolPermitted(req.Capabilities, tool) {
			return nil, fmt.Errorf("MCP tool %q exceeds the role grant", tool.Name)
		}
		if seen[tool.Name] {
			return nil, fmt.Errorf("duplicate MCP tool %q", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Handle == nil {
			return nil, fmt.Errorf("MCP tool %q has no handler", tool.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema == nil {
			return nil, fmt.Errorf("MCP tool %q requires an object schema", tool.Name)
		}
		mcphost.AddTool(registry, &mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: schema},
			func(ctx context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, map[string]any, error) {
				raw, err := json.Marshal(input)
				if err != nil {
					return nil, nil, err
				}
				output, err := tool.Handle(ctx, raw)
				if err != nil {
					return nil, nil, err
				}
				if !json.Valid(output) {
					return nil, nil, errors.New("tool returned invalid JSON")
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(output)}}}, nil, nil
			}, req.Scope.Role)
	}
	return registry.NewServer(req.Scope.Role, mcp.Implementation{Name: "osmia", Version: "1"})
}

// TokenEnvironment is the variable a turn reads its MCP bearer token from.
const TokenEnvironment = "OSMIA_MCP_TOKEN"

// CoreTransport serves each server with core's MCP host: Serve, or
// mcphost.Start when Serve is nil, which listens on a fresh loopback port and
// requires a fresh bearer token. The endpoint carries that token and names
// TokenEnvironment as the variable the turn reads it from. Its URL is the
// listener's own address, which a host turn reaches; when Via is set, the URL
// names Via as the host instead, such as host.docker.internal for a container
// turn under Docker Desktop. Releasing the lease stops the server.
type CoreTransport struct {
	Serve mcphost.StartFunc
	Via   string
}

var _ MCPTransport = CoreTransport{}

func (t CoreTransport) Start(ctx context.Context, server *mcp.Server) (Endpoint, Lease, error) {
	start := t.Serve
	if start == nil {
		start = mcphost.Start
	}
	endpoint, lease, err := start(ctx, server)
	if err != nil {
		return Endpoint{}, nil, err
	}
	if endpoint.URL == "" || endpoint.Token == "" {
		return Endpoint{}, nil, errors.Join(errors.New("MCP host returned no URL or token"), lease.Close())
	}
	url := endpoint.URL
	if t.Via != "" {
		url = endpoint.Via(t.Via)
	}
	return Endpoint{URL: url, BearerTokenEnvironment: TokenEnvironment, Token: endpoint.Token},
		&releaseLease{release: func(context.Context) error { return lease.Close() }}, nil
}
