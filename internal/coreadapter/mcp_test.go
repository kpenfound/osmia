package coreadapter

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"

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
		{Name: "allowed", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`), Handle: handler},
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
		{Scope: Scope{Role: "mason"}, Capabilities: Capabilities{Tools: []string{"bad"}}, Tools: []Tool{{Name: "bad", InputSchema: json.RawMessage(`{}`)}}},
		{Scope: Scope{Role: "mason"}, Capabilities: Capabilities{Tools: []string{"bad"}}, Tools: []Tool{{Name: "bad", InputSchema: json.RawMessage(`{`), Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}}},
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

type blockedListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *blockedListener) Accept() (net.Conn, error) { <-l.closed; return nil, net.ErrClosed }
func (l *blockedListener) Close() error              { l.once.Do(func() { close(l.closed) }); return nil }
func (l *blockedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}
func TestHTTPTransportCleanup(t *testing.T) {
	for _, mode := range []string{"release", "cancel", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listener := &blockedListener{closed: make(chan struct{})}
			transport := &HTTPTransport{Listener: listener, Endpoint: Endpoint{URL: "http://127.0.0.1:1234/mcp", BearerTokenEnvironment: "MCP_TOKEN"}, Token: "scoped-token"}
			if mode == "invalid" {
				transport.Token = ""
			}
			_, lease, err := transport.Start(ctx, mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil))
			if mode == "invalid" {
				if err == nil {
					t.Fatal("missing credential accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if mode == "cancel" {
					cancel()
				}
				if err := lease.Release(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := lease.Release(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-listener.closed:
			default:
				t.Fatal("listener leaked")
			}
		})
	}
}
