package fixturekit

import (
	context "context"
	api "github.com/kpenfound/osmia/internal/api"
	net "net"
	os "os"
	filepath "path/filepath"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
)

// TailnetNode stands in for the embedded Tailscale node with a loopback TCP
// listener, so no test reaches a real tailnet. Leaving ends the connections
// it accepted, as leaving a real tailnet does.
type TailnetNode struct {
	net.Listener
	Hostname, Directory string
	NodeNames           []string
	Closed, Left        atomic.Bool
	mu                  sync.Mutex
	conns               []net.Conn
	state               api.TailnetStatus
}

func (f *TailnetNode) State(context.Context) api.TailnetStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.State == "" {
		return api.TailnetStatus{State: api.TailnetUp}
	}
	return f.state
}

func (f *TailnetNode) SetState(state api.TailnetStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

// Drop takes the tailnet away under the service: accepting fails and the
// connections it accepted end.
func (f *TailnetNode) Drop() {
	f.Close()
	f.Leave()
}

func (f *TailnetNode) Names(context.Context) []string { return f.NodeNames }

func (f *TailnetNode) Accept() (net.Conn, error) {
	conn, err := f.Listener.Accept()
	if err == nil {
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
	}
	return conn, err
}

func (f *TailnetNode) Close() error {
	f.Closed.Store(true)
	return f.Listener.Close()
}

func (f *TailnetNode) Leave() error {
	f.Left.Store(true)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, conn := range f.conns {
		conn.Close()
	}
	return nil
}

// TailnetJoiner returns a fake tailnet join function and the node it joins.
func TailnetJoiner(names ...string) (func(string, string) (*TailnetNode, error), **TailnetNode) {
	joined := new(*TailnetNode)
	join := func(hostname, dir string) (*TailnetNode, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		*joined = &TailnetNode{Listener: l, Hostname: hostname, Directory: dir, NodeNames: names}
		return *joined, nil
	}
	return join, joined
}

// WithListen appends a [listen] table with the given body to the fixture's
// top-level configuration file.
func WithListen(t *testing.T, root, body string) {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	top, err := os.ReadFile(path)
	Must(t, err)
	Must(t, os.WriteFile(path, append(top, []byte("[listen]\n"+body)...), 0600))
}
