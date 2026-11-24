package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
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
	"time"
)

func withTailnet(t *testing.T, opts *Options, names ...string) **fixturekit.TailnetNode {
	t.Helper()
	join, joined := fixturekit.TailnetJoiner(names...)
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) { return join(hostname, dir) }
	return joined
}

func withListen(t *testing.T, opts Options, body string) {
	t.Helper()
	fixturekit.WithListen(t, opts.Config.Root, body)
}

// Without listen.tailnet the service never joins a tailnet and no response
// mentions the tailnet.
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
	intakefixture.Must(t, err)
	if cfg.Effective.Listen.Tailnet != "" {
		t.Fatalf("effective listen: %+v", cfg.Effective.Listen)
	}
	h, err := c.Health(context.Background())
	intakefixture.Must(t, err)
	st, err := c.Statuses(context.Background())
	intakefixture.Must(t, err)
	if h.Tailnet != nil || st.Tailnet != nil || cfg.Tailnet != nil {
		t.Fatalf("tailnet reported without listen.tailnet: %+v %+v %+v", h.Tailnet, st.Tailnet, cfg.Tailnet)
	}
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "tailnet")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tailnet state directory without listen.tailnet: %v", err)
	}
}

// The service joins as the configured hostname with the node's private state
// under the root, listens on the node's address and reports the configured
// hostname as its effective listen.tailnet.
func TestTailnetJoinsAsTheConfiguredHostname(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	s, c := start(t, opts)
	node := *joined
	wantDir := filepath.Join(opts.Config.Root, "tailnet")
	if node.Hostname != "osmia" || node.Directory != wantDir {
		t.Fatalf("joined as %q with state in %q", node.Hostname, node.Directory)
	}
	info, err := os.Stat(wantDir)
	intakefixture.Must(t, err)
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("tailnet state directory: %v %v", info.Mode(), info.IsDir())
	}
	if s.TailnetAddr() != node.Addr().String() {
		t.Fatalf("tailnet address %q, listener %s", s.TailnetAddr(), node.Addr())
	}
	cfg, err := c.Configuration(context.Background())
	intakefixture.Must(t, err)
	if cfg.Effective.Listen.Tailnet != "osmia" {
		t.Fatalf("effective listen: %+v", cfg.Effective.Listen)
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
	remote, base := intakefixture.WebHTTP(), "http://"+s.TailnetAddr()

	for _, host := range []string{"osmia", "OSMIA:80", "osmia.", "osmia-1", "osmia-1.tail0.ts.net", "Osmia-1.Tail0.ts.net.:80", "100.101.102.103", "[fd7a:115c:a1e0::1]:80"} {
		if code, body := intakefixture.Exchange(t, remote, "GET", base+Prefix+"/health", "", "", host); code != 200 {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	for _, host := range []string{"attacker.example", "osmia.attacker.example", "osmia-2", "localhost", "tail0.ts.net"} {
		code, body := intakefixture.Exchange(t, remote, "GET", base+Prefix+"/health", "", "", host)
		if code != 403 || intakefixture.ErrorCode(t, body) != Forbidden {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	pause := `{"target":{"scope":"factory"},"mode":"soft","reason":"travel","source":"owner"}`
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		code, body := intakefixture.Exchange(t, remote, "PUT", base+Prefix+"/runtime/pause", contentType, pause, "osmia")
		if code != 403 || intakefixture.ErrorCode(t, body) != Forbidden {
			t.Fatalf("content type %q: %d %s", contentType, code, body)
		}
	}
	state, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	if len(state.Effective.Pauses) != 0 {
		t.Fatalf("a refused tailnet request changed the runtime: %+v", state.Effective)
	}
}

func TestReloadKeepsTheTailnetListener(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	node, addr := *joined, s.TailnetAddr()

	files.write(t, strings.Replace(files.topText, "\"osmia\"", "\"osmia-2\"", 1))
	reloaded, err := c.Reload(ctx)
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.tailnet"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	if now.Effective.Listen.Tailnet != "osmia" || !hasCode(now.Diagnostics, "configuration", RestartRequired) || s.TailnetAddr() != addr || *joined != node || node.Closed.Load() || node.Left.Load() {
		t.Fatalf("after reload: %+v, bound %s", now, s.TailnetAddr())
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+addr+Prefix+"/health", "", "", "osmia"); code != 200 {
		t.Fatalf("tailnet after reload: %d %s", code, body)
	}

	files.write(t, strings.Replace(files.topText, "[listen]\ntailnet = \"osmia\"\n", "", 1))
	reloaded, err = c.Reload(ctx)
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.tailnet"}) || s.TailnetAddr() != addr || node.Closed.Load() || node.Left.Load() {
		t.Fatalf("removed listen.tailnet: %+v, bound %s", reloaded, s.TailnetAddr())
	}

	files.write(t, strings.Replace(files.topText, "\"osmia\"", "\"osmia.example\"", 1))
	reloadFails(t, c, files.top, "listen.tailnet")
}

// A tailnet that cannot be joined at startup leaves the service running with
// its socket and web listener, reports the tailnet down with the reason, and
// joins once the tailnet is reachable.
func TestTailnetJoinFailureDoesNotFailStartup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	opts.TailnetRetry = 10 * time.Millisecond
	withListen(t, opts, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	var unreachable atomic.Bool
	unreachable.Store(true)
	var joined atomic.Pointer[fixturekit.TailnetNode]
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) {
		if unreachable.Load() {
			return nil, errors.New("control server unreachable")
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		node := &fixturekit.TailnetNode{Listener: l, Hostname: hostname, Directory: dir}
		joined.Store(node)
		return node, nil
	}
	s, c := start(t, opts)
	if s.TailnetAddr() != "" {
		t.Fatalf("tailnet address while down: %s", s.TailnetAddr())
	}
	want := TailnetStatus{State: TailnetDown, Reason: "control server unreachable", HTTPSState: "unavailable", HTTPSReason: "the tailnet node is unavailable: control server unreachable"}
	h, err := c.Health(ctx)
	intakefixture.Must(t, err)
	if !h.Ready || !reflect.DeepEqual(h.Tailnet, &want) {
		t.Fatalf("health: %+v", h)
	}
	st, err := c.Statuses(ctx)
	intakefixture.Must(t, err)
	cfg, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(st.Tailnet, &want) || !reflect.DeepEqual(cfg.Tailnet, &want) {
		t.Fatalf("status %+v, config %+v", st.Tailnet, cfg.Tailnet)
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.WebAddr()+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web while the tailnet is down: %d %s", code, body)
	}

	unreachable.Store(false)
	intakefixture.Soon(t, "tailnet never joined", func() bool { return joined.Load() != nil && s.TailnetAddr() != "" })
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", "osmia"); code != 200 {
		t.Fatalf("tailnet after joining: %d %s", code, body)
	}
	h, err = c.Health(ctx)
	intakefixture.Must(t, err)
	if h.Tailnet == nil || h.Tailnet.State != TailnetUp {
		t.Fatalf("health after joining: %+v", h.Tailnet)
	}
}

// A tailnet that drops while the service runs leaves the socket, the web
// listener and requests in flight alone, is reported down, and returns when
// the tailnet does.
func TestTailnetDropKeepsServingAndRejoins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	opts.TailnetRetry = 10 * time.Millisecond
	withListen(t, opts, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	var mu sync.Mutex
	var nodes []*fixturekit.TailnetNode
	var unreachable atomic.Bool
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) {
		if unreachable.Load() {
			return nil, errors.New("no route to control")
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		node := &fixturekit.TailnetNode{Listener: l, Hostname: hostname, Directory: dir}
		mu.Lock()
		nodes = append(nodes, node)
		mu.Unlock()
		return node, nil
	}
	s, c := start(t, opts)
	first := nodes[0]

	conn, err := net.Dial("tcp", s.WebAddr())
	intakefixture.Must(t, err)
	defer conn.Close()
	body := `{"role":"mason","profile":"other"}`
	_, err = fmt.Fprintf(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(body))
	intakefixture.Must(t, err)
	reader := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reader, nil)
	intakefixture.Must(t, err)
	if interim.StatusCode != 100 {
		t.Fatal(interim.StatusCode)
	}

	unreachable.Store(true)
	first.Drop()
	intakefixture.Soon(t, "tailnet never reported down", func() bool {
		h, err := c.Health(ctx)
		return err == nil && h.Tailnet != nil && h.Tailnet.State == TailnetDown
	})
	h, err := c.Health(ctx)
	intakefixture.Must(t, err)
	if !h.Ready || !strings.Contains(h.Tailnet.Reason, "no route to control") && !strings.Contains(h.Tailnet.Reason, "tailnet listener stopped") {
		t.Fatalf("health during the outage: %+v", h)
	}
	if s.TailnetAddr() != "" {
		t.Fatalf("tailnet address during the outage: %s", s.TailnetAddr())
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.WebAddr()+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web during the outage: %d %s", code, body)
	}
	_, err = io.WriteString(conn, body)
	intakefixture.Must(t, err)
	resp, err := http.ReadResponse(reader, nil)
	intakefixture.Must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("in-flight request during the outage: %d", resp.StatusCode)
	}

	unreachable.Store(false)
	intakefixture.Soon(t, "tailnet never rejoined", func() bool { return s.TailnetAddr() != "" })
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+s.TailnetAddr()+Prefix+"/health", "", "", "osmia"); code != 200 {
		t.Fatalf("tailnet after the outage: %d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(nodes) != 2 || !first.Left.Load() {
		t.Fatalf("nodes %d, dropped node left %v", len(nodes), first.Left.Load())
	}
}

// The listener reports the node's own state: connecting, waiting for login
// with the login URL, and up.
func TestTailnetReportsTheNodeState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\ntailnet_https = false\n")
	joined := withTailnet(t, &opts)
	_, c := start(t, opts)
	node := *joined
	for _, want := range []TailnetStatus{
		{State: TailnetConnecting},
		{State: TailnetNeedsLogin, LoginURL: "https://login.tailscale.com/a/abc"},
		{State: TailnetNeedsLogin},
		{State: TailnetUp},
	} {
		node.SetState(want)
		want.HTTPSState, want.HTTPSReason = "off", "disabled by listen.tailnet_https"
		h, err := c.Health(ctx)
		intakefixture.Must(t, err)
		st, err := c.Statuses(ctx)
		intakefixture.Must(t, err)
		cfg, err := c.Configuration(ctx)
		intakefixture.Must(t, err)
		for _, got := range []*TailnetStatus{h.Tailnet, st.Tailnet, cfg.Tailnet} {
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("reported %+v, want %+v", got, want)
			}
		}
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
	intakefixture.Must(t, err)
	conn, err := net.Dial("tcp", s.TailnetAddr())
	intakefixture.Must(t, err)
	defer conn.Close()
	body := `{"role":"mason","profile":"other"}`
	_, err = fmt.Fprintf(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: osmia\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(body))
	intakefixture.Must(t, err)
	reader := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reader, nil)
	intakefixture.Must(t, err)
	if interim.StatusCode != 100 {
		t.Fatal(interim.StatusCode)
	}
	s.cancel()
	_, err = io.WriteString(conn, body)
	intakefixture.Must(t, err)
	resp, err := http.ReadResponse(reader, nil)
	intakefixture.Must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	intakefixture.Must(t, s.Wait())
	if node := *joined; !node.Closed.Load() || !node.Left.Load() {
		t.Fatalf("after shutdown: closed %v, left %v", node.Closed.Load(), node.Left.Load())
	}
}
