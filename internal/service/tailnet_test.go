package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/runtime"
)

// fakeTailnet stands in for the embedded Tailscale node with a loopback TCP
// listener, so no test reaches a real tailnet. Leaving ends the connections
// it accepted, as leaving a real tailnet does.
type fakeTailnet struct {
	net.Listener
	hostname, dir string
	names         []string
	closed, left  atomic.Bool
	mu            sync.Mutex
	conns         []net.Conn
}

func (f *fakeTailnet) Names(context.Context) []string { return f.names }
func (f *fakeTailnet) Accept() (net.Conn, error) {
	conn, err := f.Listener.Accept()
	if err == nil {
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
	}
	return conn, err
}
func (f *fakeTailnet) Close() error {
	f.closed.Store(true)
	return f.Listener.Close()
}
func (f *fakeTailnet) Leave() error {
	f.left.Store(true)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, conn := range f.conns {
		conn.Close()
	}
	return nil
}

// withTailnet sets opts to join a fake tailnet that reports names, and
// returns a pointer to the node the service joins.
func withTailnet(t *testing.T, opts *Options, names ...string) **fakeTailnet {
	t.Helper()
	joined := new(*fakeTailnet)
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		*joined = &fakeTailnet{Listener: l, hostname: hostname, dir: dir, names: names}
		return *joined, nil
	}
	return joined
}

// withListen appends a [listen] table with the given body to the fixture's
// top-level file.
func withListen(t *testing.T, opts Options, body string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte("[listen]\n"+body)...), 0600))
}

// Without listen.tailnet the service never joins a tailnet.
func TestTailnetDisabledByDefault(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	opts.JoinTailnet = func(string, string) (TailnetListener, error) {
		t.Error("joined a tailnet without listen.tailnet")
		return nil, errors.New("unexpected")
	}
	s, c := start(t, opts)
	if s.TailnetAddr() != "" {
		t.Fatalf("tailnet listener without listen.tailnet: %s", s.TailnetAddr())
	}
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	if cfg.Effective.Listen.Tailnet != "" {
		t.Fatalf("effective listen: %+v", cfg.Effective.Listen)
	}
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "tailnet")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tailnet state directory without listen.tailnet: %v", err)
	}
}

// The service joins as the configured hostname with the node's state under
// the root, and the tailnet listener serves the socket's routes with the
// socket's responses beside the loopback web listener.
func TestTailnetListenerServesTheSocketAPI(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	s, c := start(t, opts)
	node := *joined
	wantDir := filepath.Join(opts.Config.Root, "tailnet")
	if node.hostname != "osmia" || node.dir != wantDir {
		t.Fatalf("joined as %q with state in %q", node.hostname, node.dir)
	}
	info, err := os.Stat(wantDir)
	must(t, err)
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("tailnet state directory: %v %v", info.Mode(), info.IsDir())
	}
	if s.TailnetAddr() != node.Addr().String() {
		t.Fatalf("tailnet address %q, listener %s", s.TailnetAddr(), node.Addr())
	}
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	if cfg.Effective.Listen.Tailnet != "osmia" {
		t.Fatalf("effective listen: %+v", cfg.Effective.Listen)
	}

	socket, remote := socketHTTP(s.Socket()), webHTTP()
	base := "http://" + s.TailnetAddr()
	for _, path := range []string{"/health", "/config", "/runtime", "/nowhere"} {
		sc, sb := exchange(t, socket, "GET", "http://osmia"+Prefix+path, "", "", "")
		tc, tb := exchange(t, remote, "GET", base+Prefix+path, "", "", "osmia")
		if sc != tc || !bytes.Equal(sb, tb) {
			t.Fatalf("%s: socket %d %s, tailnet %d %s", path, sc, sb, tc, tb)
		}
	}
	if code, body := exchange(t, remote, "GET", "http://"+s.WebAddr()+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web beside tailnet: %d %s", code, body)
	}

	pause, err := json.Marshal(PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "travel", Source: "owner"})
	must(t, err)
	code, body := exchange(t, remote, "PUT", base+Prefix+"/runtime/pause", "application/json", string(pause), "osmia")
	if code != 200 {
		t.Fatalf("pause over tailnet: %d %s", code, body)
	}
	state, err := c.Runtime(context.Background())
	must(t, err)
	if len(state.Effective.Pauses) != 1 {
		t.Fatalf("pause written over the tailnet is not seen over the socket: %+v", state.Effective)
	}
}

// The tailnet listener answers to its hostname, the names the node reports
// and IP addresses, and refuses other hosts and writes without a JSON content
// type, as a browser would send them on another site's behalf.
func TestTailnetListenerRefusesCrossSiteRequests(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	withTailnet(t, &opts, "osmia-1.tail0.ts.net.", "osmia-1")
	s, c := start(t, opts)
	remote, base := webHTTP(), "http://"+s.TailnetAddr()

	for _, host := range []string{"osmia", "OSMIA:80", "osmia.", "osmia-1", "osmia-1.tail0.ts.net", "Osmia-1.Tail0.ts.net.:80", "100.101.102.103", "[fd7a:115c:a1e0::1]:80"} {
		if code, body := exchange(t, remote, "GET", base+Prefix+"/health", "", "", host); code != 200 {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	for _, host := range []string{"attacker.example", "osmia.attacker.example", "osmia-2", "localhost", "tail0.ts.net"} {
		code, body := exchange(t, remote, "GET", base+Prefix+"/health", "", "", host)
		if code != 403 || errorCode(t, body) != Forbidden {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	pause := `{"target":{"scope":"factory"},"mode":"soft","reason":"travel","source":"owner"}`
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		code, body := exchange(t, remote, "PUT", base+Prefix+"/runtime/pause", contentType, pause, "osmia")
		if code != 403 || errorCode(t, body) != Forbidden {
			t.Fatalf("content type %q: %d %s", contentType, code, body)
		}
	}
	state, err := c.Runtime(context.Background())
	must(t, err)
	if len(state.Effective.Pauses) != 0 {
		t.Fatalf("a refused tailnet request changed the runtime: %+v", state.Effective)
	}
}

// A changed or removed listen.tailnet keeps the joined node and is named in
// restart_required; an invalid one fails the reload by its field.
func TestReloadKeepsTheTailnetListener(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	node, addr := *joined, s.TailnetAddr()

	files.write(t, strings.Replace(files.topText, "\"osmia\"", "\"osmia-2\"", 1), files.projectText)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.tailnet"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.Effective.Listen.Tailnet != "osmia" || !hasCode(now.Diagnostics, "configuration", RestartRequired) || s.TailnetAddr() != addr || *joined != node || node.closed.Load() || node.left.Load() {
		t.Fatalf("after reload: %+v, bound %s", now, s.TailnetAddr())
	}
	if code, body := exchange(t, webHTTP(), "GET", "http://"+addr+Prefix+"/health", "", "", "osmia"); code != 200 {
		t.Fatalf("tailnet after reload: %d %s", code, body)
	}

	files.write(t, strings.Replace(files.topText, "[listen]\ntailnet = \"osmia\"\n", "", 1), files.projectText)
	reloaded, err = c.Reload(ctx)
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.tailnet"}) || s.TailnetAddr() != addr || node.closed.Load() || node.left.Load() {
		t.Fatalf("removed listen.tailnet: %+v, bound %s", reloaded, s.TailnetAddr())
	}

	files.write(t, strings.Replace(files.topText, "\"osmia\"", "\"osmia.example\"", 1), files.projectText)
	reloadFails(t, c, files.top, "listen.tailnet")
}

// A tailnet that cannot be joined fails startup and leaves neither the
// socket, the web listener nor the root lock behind.
func TestTailnetJoinFailureFailsStartup(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	web := reserved.Addr().String()
	must(t, reserved.Close())
	withListen(t, opts, "web = \""+web+"\"\ntailnet = \"osmia\"\n")
	opts.JoinTailnet = func(string, string) (TailnetListener, error) {
		return nil, errors.New("control server unreachable")
	}
	s, err := Start(context.Background(), opts)
	if err == nil {
		s.Close()
		t.Fatal("started without joining the tailnet")
	}
	if !strings.Contains(err.Error(), "listen.tailnet") || !strings.Contains(err.Error(), "control server unreachable") {
		t.Fatalf("join error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(opts.Config.Root, "osmia.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left after failed startup: %v", err)
	}
	// Starting again on the same web address fails if the first attempt
	// left its listener bound.
	withTailnet(t, &opts)
	s, _ = start(t, opts)
	if s.WebAddr() != web {
		t.Fatalf("web listener bound %s, configured %s", s.WebAddr(), web)
	}
}

// Shutdown drains a request in flight over the tailnet before it leaves the
// tailnet.
func TestShutdownDrainsTailnetRequestsBeforeLeaving(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	s, err := Start(context.Background(), opts)
	must(t, err)
	conn, err := net.Dial("tcp", s.TailnetAddr())
	must(t, err)
	defer conn.Close()
	body := `{"role":"mason","profile":"other"}`
	_, err = fmt.Fprintf(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: osmia\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(body))
	must(t, err)
	reader := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reader, nil)
	must(t, err)
	if interim.StatusCode != 100 {
		t.Fatal(interim.StatusCode)
	}
	s.cancel()
	_, err = io.WriteString(conn, body)
	must(t, err)
	resp, err := http.ReadResponse(reader, nil)
	must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	must(t, s.Wait())
	if node := *joined; !node.closed.Load() || !node.left.Load() {
		t.Fatalf("after shutdown: closed %v, left %v", node.closed.Load(), node.left.Load())
	}
}

// Shutdown leaves the tailnet before Close returns.
func TestShutdownLeavesTheTailnet(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	s, err := Start(context.Background(), opts)
	must(t, err)
	if code, _ := exchange(t, webHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", "osmia"); code != 200 {
		t.Fatalf("tailnet before shutdown: %d", code)
	}
	must(t, s.Close())
	if !(*joined).closed.Load() || !(*joined).left.Load() {
		t.Fatal("tailnet node open after shutdown")
	}
}
