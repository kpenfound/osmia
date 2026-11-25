package coreadapter

import (
	"context"
	"net"
	"testing"

	"github.com/kpenfound/agentcore/agent"
)

func TestListenMCPOpensTheListenerATurnReachesItsToolsBy(t *testing.T) {
	previous := hostOS
	hostOS = "darwin"
	t.Cleanup(func() { hostOS = previous })
	for _, tc := range []struct {
		mode string
		via  string
	}{{agent.SandboxNone, ""}, {agent.SandboxClaude, ""}, {agent.SandboxSbx, ContainerHost}, {agent.SandboxContainer, ContainerHost}} {
		listener, address, err := ListenMCP(context.Background(), tc.mode, "no-such-engine")
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		host, port, err := net.SplitHostPort(address)
		_, listening, _ := net.SplitHostPort(listener.Addr().String())
		if err != nil || port != listening || listener.Addr().(*net.TCPAddr).IP.String() != "127.0.0.1" {
			t.Errorf("%s: dials %q, listens on %s", tc.mode, address, listener.Addr())
		}
		if tc.via != "" && host != tc.via || tc.via == "" && host != "127.0.0.1" {
			t.Errorf("%s: dials host %q", tc.mode, host)
		}
		_ = listener.Close()
	}
	hostOS = "linux"
	if listener, _, err := ListenMCP(context.Background(), agent.SandboxContainer, "no-such-engine"); err == nil {
		_ = listener.Close()
		t.Fatal("a container turn on Linux listened without finding the engine's bridge gateway")
	}
}
