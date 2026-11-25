package coreadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTransport serves a server to one turn and returns where the turn reaches
// it, with the bearer token the endpoint requires. Start owns cleanup if
// startup fails; a successful lease closes the transport.
type MCPTransport interface {
	Start(context.Context, *mcp.Server) (Endpoint, Lease, error)
}

// MCPHost selects a transport for the turn's execution mode. Sbx requires its
// own transport because its proxy reaches host loopback on every platform.
type MCPHost struct{ Transport, Container, Sbx MCPTransport }

var _ MCPHosts = (*MCPHost)(nil)

func (h *MCPHost) Host(ctx context.Context, req HostRequest) (HostedMCP, error) {
	if err := ctx.Err(); err != nil {
		return HostedMCP{}, err
	}
	server, err := scopedServer(req)
	if err != nil {
		return HostedMCP{}, err
	}
	transport := h.Transport
	if req.Execution.Mode == agent.SandboxContainer && h.Container != nil {
		transport = h.Container
	}
	if req.Execution.Mode == agent.SandboxSbx {
		transport = h.Sbx
	}
	if transport == nil {
		return HostedMCP{}, unsupported("MCP transport", "no transport supplied")
	}
	endpoint, lease, err := transport.Start(ctx, server)
	if err != nil {
		return HostedMCP{}, err
	}
	return HostedMCP{Endpoint: endpoint, Lease: lease}, nil
}

func scopedServer(req HostRequest) (server *mcp.Server, err error) {
	if req.Scope.Role == "" {
		return nil, errors.New("MCP host requires a role")
	}
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
			func(ctx context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
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

// ContainerHost is the name a container turn reaches the host by. Core's
// container sessions resolve it on macOS and Linux.
const ContainerHost = "host.docker.internal"

// SbxTransport serves on loopback and advertises the sandbox proxy's host
// name. It never inspects a Docker bridge or opens a wildcard listener.
func SbxTransport() CoreTransport { return CoreTransport{Via: ContainerHost} }

var hostOS = runtime.GOOS

// ContainerTransport is the CoreTransport of container turns: each server
// listens where a container reaches the host, the loopback on macOS and the
// bridge gateway of the container engine on Linux, and its URL names
// ContainerHost. engine is the container engine's executable, asked for the
// gateway when a turn starts.
func ContainerTransport(engine string) CoreTransport {
	return CoreTransport{Via: ContainerHost, Serve: func(ctx context.Context, server *mcp.Server) (mcphost.Endpoint, mcphost.Lease, error) {
		if hostOS != "linux" {
			return mcphost.Start(ctx, server)
		}
		gateway, err := bridgeGateway(ctx, engine)
		if err != nil {
			return mcphost.Endpoint{}, nil, err
		}
		return mcphost.StartOn(ctx, server, net.JoinHostPort(gateway, "0"))
	}}
}

func bridgeGateway(ctx context.Context, engine string) (string, error) {
	out, err := exec.CommandContext(ctx, engine, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		return "", fmt.Errorf("find the address a container reaches the host by (%s network inspect bridge): %w", engine, err)
	}
	gateway := strings.TrimSpace(string(out))
	if net.ParseIP(gateway) == nil {
		return "", fmt.Errorf("%s network inspect bridge reported %q, not an address", engine, gateway)
	}
	return gateway, nil
}

// ListenMCP opens the listener a turn run in mode reaches its tools by and
// returns it with the host and port the turn dials. A host turn dials the
// loopback address itself, an sbx turn the name its sandbox proxy resolves to
// the host, and a container turn on Linux the bridge gateway of containerEngine,
// where a container reaches the host. A client's local MCP listener for a
// remote turn is opened where the service would open the turn's own.
func ListenMCP(ctx context.Context, mode, containerEngine string) (net.Listener, string, error) {
	address, via := "127.0.0.1:0", ""
	switch mode {
	case agent.SandboxSbx:
		via = ContainerHost
	case agent.SandboxContainer:
		via = ContainerHost
		if hostOS == "linux" {
			gateway, err := bridgeGateway(ctx, containerEngine)
			if err != nil {
				return nil, "", err
			}
			address = net.JoinHostPort(gateway, "0")
		}
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, "", err
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return nil, "", errors.Join(err, listener.Close())
	}
	host := listener.Addr().String()
	if via != "" {
		host = net.JoinHostPort(via, port)
	}
	return listener, host, nil
}
