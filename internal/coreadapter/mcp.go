package coreadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"

	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTransport binds a server to caller-selected transport and credentials.
// Start owns cleanup if startup fails; a successful lease closes the transport.
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
	for _, tool := range req.Tools {
		if !slices.Contains(req.Capabilities.Tools, tool.Name) {
			continue
		}
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

// HTTPTransport serves an already-bound listener using core's authenticated MCP
// host. The caller owns address selection and passes the token through its scoped
// credential boundary. Each transport instance is used for one Host call.
type HTTPTransport struct {
	Listener net.Listener
	Endpoint Endpoint
	Token    string
}

func (h *HTTPTransport) Start(ctx context.Context, server *mcp.Server) (Endpoint, Lease, error) {
	if h.Listener == nil {
		return Endpoint{}, nil, errors.New("MCP HTTP listener is missing")
	}
	parsed, err := url.Parse(h.Endpoint.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || h.Token == "" || h.Endpoint.BearerTokenEnvironment == "" {
		return Endpoint{}, nil, errors.Join(errors.New("MCP HTTP requires a URL, token and token environment name"), h.Listener.Close())
	}
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	var serveErr error
	go func() { defer close(finished); serveErr = mcphost.ServeHTTP(runCtx, server, h.Listener, h.Token) }()
	lease := &releaseLease{release: func(context.Context) error { cancel(); <-finished; return serveErr }}
	return h.Endpoint, lease, nil
}
