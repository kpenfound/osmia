package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/runtime"
)

// withWeb appends a listen.web setting to the fixture's top-level file.
func withWeb(t *testing.T, opts Options, addr string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte("[listen]\nweb = \""+addr+"\"\n")...), 0600))
}

// exchange sends one raw request through client and returns its status and
// body.
func exchange(t *testing.T, client *http.Client, method, url, contentType, body string, host string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	must(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	must(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	must(t, err)
	return resp.StatusCode, data
}

func socketHTTP(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func webHTTP() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

func errorCode(t *testing.T, body []byte) Code {
	t.Helper()
	var out ErrorResponse
	must(t, json.Unmarshal(body, &out))
	return out.Error.Code
}

// Without listen.web the service binds only its Unix socket.
func TestWebListenerDisabledByDefault(t *testing.T) {
	t.Parallel()
	s, c := start(t, fixture(t))
	if s.WebAddr() != "" {
		t.Fatalf("web listener bound without listen.web: %s", s.WebAddr())
	}
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	if cfg.Effective.Listen.Web != "" {
		t.Fatalf("effective listen: %+v", cfg.Effective.Listen)
	}
}

// A loopback web listener serves the socket's routes with the socket's
// responses, and a write through either is seen through the other.
func TestWebListenerServesTheSocketAPI(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	addr := s.WebAddr()
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		t.Fatalf("web address %q", addr)
	}
	socket, web := socketHTTP(s.Socket()), webHTTP()
	base := "http://" + addr
	for _, path := range []string{"/health", "/config", "/runtime", "/nowhere"} {
		sc, sb := exchange(t, socket, "GET", "http://osmia"+Prefix+path, "", "", "")
		wc, wb := exchange(t, web, "GET", base+Prefix+path, "", "", "")
		if sc != wc || !bytes.Equal(sb, wb) {
			t.Fatalf("%s: socket %d %s, web %d %s", path, sc, sb, wc, wb)
		}
	}

	pause, err := json.Marshal(PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "travel", Source: "owner"})
	must(t, err)
	code, body := exchange(t, web, "PUT", base+Prefix+"/runtime/pause", "application/json; charset=utf-8", string(pause), "")
	if code != 200 {
		t.Fatalf("pause over web: %d %s", code, body)
	}
	state, err := c.Runtime(context.Background())
	must(t, err)
	if len(state.Effective.Pauses) != 1 {
		t.Fatalf("pause written over web is not seen over the socket: %+v", state.Effective)
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
	web, base := webHTTP(), "http://"+s.WebAddr()
	_, port, err := net.SplitHostPort(s.WebAddr())
	must(t, err)

	for _, host := range []string{"localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port, "LOCALHOST"} {
		if code, body := exchange(t, web, "GET", base+Prefix+"/health", "", "", host); code != 200 {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	for _, host := range []string{"attacker.example:" + port, "attacker.example", "10.0.0.1:" + port, "osmia"} {
		code, body := exchange(t, web, "GET", base+Prefix+"/health", "", "", host)
		if code != 403 || errorCode(t, body) != Forbidden {
			t.Fatalf("host %s: %d %s", host, code, body)
		}
	}
	pause := `{"target":{"scope":"factory"},"mode":"soft","reason":"travel","source":"owner"}`
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		code, body := exchange(t, web, "PUT", base+Prefix+"/runtime/pause", contentType, pause, "")
		if code != 403 || errorCode(t, body) != Forbidden {
			t.Fatalf("content type %q: %d %s", contentType, code, body)
		}
	}
	state, err := c.Runtime(context.Background())
	must(t, err)
	if len(state.Effective.Pauses) != 0 {
		t.Fatalf("a refused web request changed the runtime: %+v", state.Effective)
	}
	if code, body := exchange(t, socketHTTP(s.Socket()), "PUT", "http://attacker.example"+Prefix+"/runtime/pause", "text/plain", pause, ""); code != 200 {
		t.Fatalf("socket write: %d %s", code, body)
	}
}

// A changed or removed listen.web keeps the bound listener and is named in
// restart_required; an invalid one fails the reload by its field.
func TestReloadKeepsTheWebListener(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	files := configFiles(t, opts)
	s, c := start(t, opts)
	addr := s.WebAddr()

	files.write(t, strings.Replace(files.topText, "127.0.0.1:0", "[::1]:0", 1), files.projectText)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.web"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.Effective.Listen.Web != "127.0.0.1:0" || !hasCode(now.Diagnostics, "configuration", RestartRequired) || s.WebAddr() != addr {
		t.Fatalf("after reload: %+v, bound %s", now, s.WebAddr())
	}
	if code, body := exchange(t, webHTTP(), "GET", "http://"+addr+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web after reload: %d %s", code, body)
	}

	files.write(t, strings.Replace(files.topText, "[listen]\nweb = \"127.0.0.1:0\"\n", "", 1), files.projectText)
	reloaded, err = c.Reload(ctx)
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.web"}) || s.WebAddr() != addr {
		t.Fatalf("removed listen.web: %+v, bound %s", reloaded, s.WebAddr())
	}

	files.write(t, strings.Replace(files.topText, "127.0.0.1:0", "0.0.0.0:0", 1), files.projectText)
	reloadFails(t, c, files.top, "listen.web")
}

// A web address that cannot be bound fails startup and leaves neither the
// socket nor the root lock behind.
func TestWebListenerBindFailureFailsStartup(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
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
	must(t, err)
	must(t, os.WriteFile(path, bytes.Replace(top, []byte(taken.Addr().String()), []byte("127.0.0.1:0"), 1), 0600))
	start(t, opts)
}

// Shutdown stops both servers before Close returns.
func TestShutdownClosesBothListeners(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, err := Start(context.Background(), opts)
	must(t, err)
	addr, socket := s.WebAddr(), s.Socket()
	if code, _ := exchange(t, webHTTP(), "GET", "http://"+addr+Prefix+"/health", "", "", ""); code != 200 {
		t.Fatalf("web before shutdown: %d", code)
	}
	must(t, s.Close())
	// Dialing the freed port could reach another test's listener, so ask the
	// listener itself whether it is closed.
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
