package coreadapter

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type memoryTransport struct {
	client *mcp.ClientSession
	starts int
}

func (f *memoryTransport) Start(ctx context.Context, server *mcp.Server) (Endpoint, Lease, error) {
	f.starts++
	serverSide, clientSide := mcp.NewInMemoryTransports()
	session, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		return Endpoint{}, nil, err
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		_ = session.Close()
		return Endpoint{}, nil, err
	}
	f.client = client
	return Endpoint{URL: "http://fake/mcp"}, &releaseLease{release: func(context.Context) error { _ = client.Close(); return session.Close() }}, nil
}
func TestMCPRoleScopedCalls(t *testing.T) {
	ctx := context.Background()
	calls := 0
	handler := func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { calls++; return input, nil }
	request := HostRequest{Scope: Scope{Role: "mason"}, Capabilities: Capabilities{Tools: []string{"allowed", "unregistered"}}, Tools: []Tool{
		{Name: "allowed", Effect: ToolRead, InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`), Handle: handler},
		{Name: "hidden", InputSchema: json.RawMessage(`{"type":"object"}`), Handle: handler},
	}}
	transport := &memoryTransport{}
	hosted, err := (&MCPHost{Transport: transport}).Host(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer hosted.Lease.Release(ctx)
	listed, err := transport.client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "allowed" {
		t.Fatalf("tools: %+v", listed)
	}
	for _, name := range []string{"allowed", "hidden", "unregistered", "unknown"} {
		result, err := transport.client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"value": "ok"}})
		if name == "allowed" {
			if err != nil || result.IsError {
				t.Fatalf("%v %v", result, err)
			}
		} else if err == nil && !result.IsError {
			t.Fatalf("%s was callable", name)
		}
	}
	result, err := transport.client.CallTool(ctx, &mcp.CallToolParams{Name: "allowed", Arguments: map[string]any{"value": 12}})
	if err == nil && !result.IsError {
		t.Fatal("schema not enforced")
	}
	if calls != 1 {
		t.Fatalf("handler calls: %d", calls)
	}
}
func TestMCPRejectsInvalidBeforeHosting(t *testing.T) {
	for _, req := range []HostRequest{
		{},
		{Scope: Scope{Role: "mason"}, Capabilities: Capabilities{Tools: []string{"bad"}}, Tools: []Tool{{Name: "bad", Effect: ToolRead, InputSchema: json.RawMessage(`{}`)}}},
		{Scope: Scope{Role: "mason"}, Capabilities: Capabilities{Tools: []string{"bad"}}, Tools: []Tool{{Name: "bad", Effect: ToolRead, InputSchema: json.RawMessage(`{`), Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}}},
	} {
		transport := &memoryTransport{}
		_, err := (&MCPHost{Transport: transport}).Host(context.Background(), req)
		if err == nil || transport.starts != 0 {
			t.Fatalf("err=%v starts=%d", err, transport.starts)
		}
	}
}
func TestMCPEmptyAllowlistExposesNothing(t *testing.T) {
	transport := &memoryTransport{}
	host, err := (&MCPHost{Transport: transport}).Host(context.Background(), HostRequest{Scope: Scope{Role: "committee"}, Tools: []Tool{{Name: "hidden"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Lease.Release(context.Background())
	listed, err := transport.client.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 0 {
		t.Fatalf("%+v %v", listed, err)
	}
}

func TestMCPRejectsEffectEscalationAndDuplicates(t *testing.T) {
	for _, effect := range []ToolEffect{ToolWrite, ToolExecute, ToolFetch, ToolVCS, "", "unknown"} {
		transport := &memoryTransport{}
		tool := Tool{Name: "apparently_read_only", Effect: effect, InputSchema: json.RawMessage(`{"type":"object"}`), Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			t.Fatal("denied handler called")
			return nil, nil
		}}
		_, err := (&MCPHost{Transport: transport}).Host(context.Background(), HostRequest{Scope: Scope{Role: "committee"}, Capabilities: Capabilities{Tools: []string{tool.Name}}, Tools: []Tool{tool}})
		if err == nil || transport.starts != 0 {
			t.Fatalf("effect %q reached transport: %v", effect, err)
		}
	}
	transport := &memoryTransport{}
	tool := Tool{Name: "read", Effect: ToolRead, InputSchema: json.RawMessage(`{"type":"object"}`), Handle: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) { return raw, nil }}
	_, err := (&MCPHost{Transport: transport}).Host(context.Background(), HostRequest{Scope: Scope{Role: "committee"}, Capabilities: Capabilities{Tools: []string{tool.Name}}, Tools: []Tool{tool, tool}})
	if err == nil || transport.starts != 0 {
		t.Fatal("duplicate tool accepted")
	}
}

// A role-scoped server hosted on core's loopback transport is reached through
// the returned endpoint with its token, and only with it, until the lease is
// released.
func TestCoreTransportServesUntilReleased(t *testing.T) {
	ctx := context.Background()
	tool := Tool{Name: "notes_read", Effect: ToolRead, InputSchema: json.RawMessage(`{"type":"object"}`),
		Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"notes"`), nil
		}}
	hosted, err := (&MCPHost{Transport: CoreTransport{}}).Host(ctx, HostRequest{Scope: Scope{Role: "architect"}, Capabilities: Capabilities{Tools: []string{"notes_read"}}, Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := hosted.Endpoint
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || endpoint.Token == "" || endpoint.BearerTokenEnvironment != TokenEnvironment {
		t.Fatalf("endpoint: %+v", endpoint)
	}
	connect := func(token string) (*mcp.ClientSession, error) {
		return mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: endpoint.URL, HTTPClient: &http.Client{Transport: mcphost.BearerTransport(token, nil)}, MaxRetries: -1}, nil)
	}
	client, err := connect(endpoint.Token)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := client.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "notes_read" {
		t.Fatalf("tools: %+v %v", listed, err)
	}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "notes_read", Arguments: map[string]any{}})
	if err != nil || result.IsError || result.Content[0].(*mcp.TextContent).Text != `"notes"` {
		t.Fatalf("call: %+v %v", result, err)
	}
	_ = client.Close()
	if _, err := connect("other-token"); err == nil {
		t.Fatal("another token reached the server")
	}
	if err := hosted.Lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := hosted.Lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", parsed.Host); err == nil {
		conn.Close()
		t.Fatal("server still listens after release")
	}
}

func TestCoreTransportRejectsAnEndpointWithoutToken(t *testing.T) {
	closed := false
	start := func(ctx context.Context, srv *mcp.Server) (mcphost.Endpoint, mcphost.Lease, error) {
		endpoint, lease, err := mcphost.StartMemory(ctx, srv)
		endpoint.Token = ""
		return endpoint, closeTracker{lease, &closed}, err
	}
	_, lease, err := (CoreTransport{Serve: start}).Start(context.Background(), mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil))
	if err == nil || lease != nil || !closed {
		t.Fatalf("lease=%v err=%v closed=%v", lease, err, closed)
	}
}

type closeTracker struct {
	mcphost.Lease
	closed *bool
}

func (c closeTracker) Close() error { *c.closed = true; return c.Lease.Close() }
