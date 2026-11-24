package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func withWeb(t *testing.T, opts Options, addr string) {
	t.Helper()
	intakefixture.WithWeb(t, opts.Config.Root, addr)
}

func socketHTTP(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

// Without listen.web the service binds only its Unix socket.
func TestWebListenerDisabledByDefault(t *testing.T) {
	t.Parallel()
	s, c := start(t, fixture(t))
	if s.WebAddr() != "" {
		t.Fatalf("web listener bound without listen.web: %s", s.WebAddr())
	}
	cfg, err := c.Configuration(context.Background())
	fixturekit.Must(t, err)
	if cfg.Effective.Listen.Web != "" {
		t.Fatalf("effective listen: %+v", cfg.Effective.Listen)
	}
}

// A loopback web listener serves the socket's API: a write through it is seen
// through the socket, and a path neither the page nor the API holds gets the
// API's answer.
func TestWebListenerServesTheSocketAPI(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	addr := s.WebAddr()
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		t.Fatalf("web address %q", addr)
	}
	web, base := intakefixture.WebHTTP(), "http://"+addr
	pause, err := json.Marshal(PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "travel", Source: "owner"})
	fixturekit.Must(t, err)
	code, body := intakefixture.Exchange(t, web, "PUT", base+Prefix+"/runtime/pause", "application/json; charset=utf-8", string(pause), "")
	if code != 200 {
		t.Fatalf("pause over web: %d %s", code, body)
	}
	state, err := c.Runtime(context.Background())
	fixturekit.Must(t, err)
	if len(state.Effective.Pauses) != 1 {
		t.Fatalf("pause written over web is not seen over the socket: %+v", state.Effective)
	}
	code, body = intakefixture.Exchange(t, web, "GET", base+"/index.html", "", "", "")
	if code != 404 || intakefixture.ErrorCode(t, body) != NotFound {
		t.Fatalf("unknown page path: %d %s", code, body)
	}
}

// The web listener refuses what a browser would send on another site's
// behalf: a Host that is not loopback, and a write without a JSON content
// type. The socket accepts both.
func TestWebListenerRefusesCrossSiteRequests(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withWeb(t, opts, "localhost:0")
	s, c := start(t, opts)
	web, base := intakefixture.WebHTTP(), "http://"+s.WebAddr()
	_, port, err := net.SplitHostPort(s.WebAddr())
	fixturekit.Must(t, err)

	for _, host := range []string{"localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port, "LOCALHOST"} {
		if code, body := intakefixture.Exchange(t, web, "GET", base+Prefix+"/health", "", "", host); code != 200 {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	for _, host := range []string{"attacker.example:" + port, "attacker.example", "10.0.0.1:" + port, "osmia"} {
		code, body := intakefixture.Exchange(t, web, "GET", base+Prefix+"/health", "", "", host)
		if code != 403 || intakefixture.ErrorCode(t, body) != Forbidden {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	pause := `{"target":{"scope":"factory"},"mode":"soft","reason":"travel","source":"owner"}`
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		code, body := intakefixture.Exchange(t, web, "PUT", base+Prefix+"/runtime/pause", contentType, pause, "")
		if code != 403 || intakefixture.ErrorCode(t, body) != Forbidden {
			t.Fatalf("content type %q: %d %s", contentType, code, body)
		}
	}
	state, err := c.Runtime(context.Background())
	fixturekit.Must(t, err)
	if len(state.Effective.Pauses) != 0 {
		t.Fatalf("a refused web request changed the runtime: %+v", state.Effective)
	}
	if code, body := intakefixture.Exchange(t, socketHTTP(s.Socket()), "PUT", "http://attacker.example"+Prefix+"/runtime/pause", "text/plain", pause, ""); code != 200 {
		t.Fatalf("socket write: %d %s", code, body)
	}
}

func TestReloadKeepsTheWebListener(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	files := configFiles(t, opts)
	s, c := start(t, opts)
	addr := s.WebAddr()

	files.write(t, strings.Replace(files.topText, "127.0.0.1:0", "[::1]:0", 1))
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.web"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Effective.Listen.Web != "127.0.0.1:0" || !hasCode(now.Diagnostics, "configuration", RestartRequired) || s.WebAddr() != addr {
		t.Fatalf("after reload: %+v, bound %s", now, s.WebAddr())
	}
	if code, body := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+addr+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web after reload: %d %s", code, body)
	}

	files.write(t, strings.Replace(files.topText, "[listen]\nweb = \"127.0.0.1:0\"\n", "", 1))
	reloaded, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.web"}) || s.WebAddr() != addr {
		t.Fatalf("removed listen.web: %+v, bound %s", reloaded, s.WebAddr())
	}

	files.write(t, strings.Replace(files.topText, "127.0.0.1:0", "0.0.0.0:0", 1))
	reloadFails(t, c, files.top, "listen.web")
}

// A web address that cannot be bound fails startup and leaves neither the
// socket nor the root lock behind.
func TestWebListenerBindFailureFailsStartup(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	fixturekit.Must(t, err)
	defer taken.Close()
	opts := fixture(t)
	withWeb(t, opts, taken.Addr().String())
	s, err := Start(context.Background(), opts)
	if err == nil {
		s.Close()
		t.Fatal("started with an address already in use")
	}
	if !strings.Contains(err.Error(), "listen.web") {
		t.Fatalf("bind error does not name listen.web: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(opts.Config.Root, "osmia.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left after failed startup: %v", err)
	}
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, bytes.Replace(top, []byte(taken.Addr().String()), []byte("127.0.0.1:0"), 1), 0600))
	start(t, opts)
}

// Shutdown stops both servers before Close returns.
func TestShutdownClosesBothListeners(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, err := Start(context.Background(), opts)
	fixturekit.Must(t, err)
	addr, socket := s.WebAddr(), s.Socket()
	if code, _ := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", "http://"+addr+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web before shutdown: %d", code)
	}
	fixturekit.Must(t, s.Close())
	web := s.web.(*net.TCPListener)
	web.SetDeadline(time.Now().Add(time.Second))
	if conn, err := web.Accept(); !errors.Is(err, net.ErrClosed) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("web listener open after shutdown: %v", err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after shutdown: %v", err)
	}
}
