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
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

const project config.ProjectID = "p_0123456789abcdef0123456789abcdef"
const stream config.WorkstreamID = "w_0123456789abcdef0123456789abcdef"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) Options {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	return fixtureAt(t, home)
}

// fixtureAt writes a minimal root and project configuration under home.
func fixtureAt(t *testing.T, home string) Options {
	t.Helper()
	root := filepath.Join(home, "root")
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(project)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version = 1
active_projects = [%q]
[profiles.default]
agent = "claude"
model = "test"
[profiles.other]
agent = "codex"
model = "other"
`, project)), 0600))
	must(t, os.WriteFile(filepath.Join(root, "projects", string(project), "config.toml"), []byte(`version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "`+filepath.Join(home, "clone")+`"
`), 0600))
	return Options{Config: config.Options{Root: root}, Build: Identity{"test", "abc"}, Workstreams: []config.WorkstreamID{stream}, ShutdownTimeout: 100 * time.Millisecond}
}
func start(t *testing.T, opts Options) (*Service, *Client) {
	t.Helper()
	s, err := Start(context.Background(), opts)
	must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close(); s.Close() })
	return s, c
}
func mutation(t *testing.T, c *Client, method, kind string, input any) {
	t.Helper()
	var result MutationResponse
	must(t, c.Do(context.Background(), method, Prefix+"/runtime/"+kind, input, &result))
	if !result.Applied {
		t.Fatal("not acknowledged")
	}
}
func TestRoundTrip(t *testing.T) {
	opts := fixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	health, err := c.Health(ctx)
	must(t, err)
	if !health.Ready || health.Build != opts.Build || health.APIVersion != 1 {
		t.Fatal(health)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Effective.Project.ID != project || cfg.Root != s.cfg.Root.String() || len(cfg.Digest) != 64 || len(cfg.Diagnostics) != 0 || cfg.Project.CharterState != nil {
		t.Fatalf("%+v", cfg)
	}
	// Without a trace there is no charter to gate on.
	var api *APIError
	if _, err := c.HandIn(ctx, HandInRequest{Project: project}); !errors.As(err, &api) || api.Code != Internal || !strings.Contains(api.Message, "no trace repository") {
		t.Fatalf("hand-in without a trace: %v", err)
	}
	info, err := os.Stat(s.Socket())
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: "operator"})
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{stream}})
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	before, err := c.Runtime(ctx)
	must(t, err)
	if len(before.Effective.Pauses) != 1 || len(before.Effective.Priorities) != 1 || before.Effective.Profiles["mason"] != "other" {
		t.Fatal(before)
	}
	must(t, s.Close())
	if _, err := os.Lstat(s.Socket()); !os.IsNotExist(err) {
		t.Fatal("socket retained", err)
	}
	s2, c2 := start(t, opts)
	after, err := c2.Runtime(ctx)
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal(before, after)
	}
	mutation(t, c2, "DELETE", "pause", ClearPauseRequest{Scope: "factory"})
	mutation(t, c2, "DELETE", "priority", ClearPriorityRequest{project})
	mutation(t, c2, "DELETE", "profile", ClearProfileRequest{"mason"})
	after, err = c2.Runtime(ctx)
	must(t, err)
	if len(after.Effective.Pauses) != 0 || len(after.Effective.Priorities) != 0 || after.Effective.Profiles["mason"] != "default" {
		t.Fatal(after)
	}
	must(t, s2.Close())
	_, c3 := start(t, opts)
	cleared, err := c3.Runtime(ctx)
	must(t, err)
	if !reflect.DeepEqual(after, cleared) {
		t.Fatal("cleared overrides returned after restart")
	}
}
func TestRootContention(t *testing.T) {
	opts := fixture(t)
	s, c := start(t, opts)
	before, err := os.Lstat(s.Socket())
	must(t, err)
	alias := filepath.Join(filepath.Dir(opts.Config.Root), "alias")
	must(t, os.Symlink(opts.Config.Root, alias))
	opts.Config.Root = alias
	if second, err := Start(context.Background(), opts); err == nil {
		second.Close()
		t.Fatal("second owner accepted")
	} else if !strings.Contains(err.Error(), "already owned") {
		t.Fatal(err)
	}
	after, err := os.Lstat(s.Socket())
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("socket replaced")
	}
	_, err = c.Health(context.Background())
	must(t, err)
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
}
func TestStaleSocket(t *testing.T) {
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "osmia.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	must(t, err)
	l.SetUnlinkOnClose(false)
	must(t, l.Close())
	_, c := start(t, opts)
	_, err = c.Health(context.Background())
	must(t, err)
}
func TestForeignLiveSocket(t *testing.T) {
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "osmia.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	must(t, err)
	defer l.Close()
	before, err := os.Lstat(path)
	must(t, err)
	if s, err := Start(context.Background(), opts); err == nil {
		s.Close()
		t.Fatal("live socket replaced")
	}
	after, err := os.Lstat(path)
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("live socket disturbed")
	}
	conn, err := net.Dial("unix", path)
	must(t, err)
	conn.Close()
}
func TestSocketAndLockArtifacts(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			opts := fixture(t)
			path := filepath.Join(opts.Config.Root, "osmia.sock")
			if kind == "lock-symlink" {
				path = filepath.Join(opts.Config.Root, ".service.lock")
			}
			if kind == "file" {
				must(t, os.WriteFile(path, []byte("keep"), 0600))
			} else {
				must(t, os.Symlink(filepath.Join(opts.Config.Root, "config.toml"), path))
			}
			before, err := os.Lstat(path)
			must(t, err)
			if s, err := Start(context.Background(), opts); err == nil {
				s.Close()
				t.Fatal("unsafe artifact accepted")
			}
			after, err := os.Lstat(path)
			must(t, err)
			if !os.SameFile(before, after) {
				t.Fatal("artifact changed")
			}
		})
	}
}
func TestInvalidDiskRetainsView(t *testing.T) {
	opts := fixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	before, err := c.Runtime(ctx)
	must(t, err)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	path := filepath.Join(opts.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte("credential = 'secret-parser-text'\n[broken"), 0600))
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.Digest != cfg.Digest || !reflect.DeepEqual(now.Effective, cfg.Effective) || len(now.Diagnostics) != 1 {
		t.Fatal(now)
	}
	encoded, _ := json.Marshal(now)
	if bytes.Contains(encoded, []byte("secret-parser-text")) {
		t.Fatal("parser text leaked")
	}
	must(t, os.WriteFile(path, append(original, []byte("\n[capacity]\nmasons = 7\n")...), 0600))
	now, err = c.Configuration(ctx)
	must(t, err)
	if now.Diagnostics[0].Code != RestartRequired || now.Digest != cfg.Digest {
		t.Fatal(now)
	}
	path = filepath.Join(opts.Config.Root, "runtime.json")
	original, err = os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte("{invalid secret-runtime-text"), 0600))
	state, err := c.Runtime(ctx)
	must(t, err)
	if !reflect.DeepEqual(state.Effective, before.Effective) || len(state.Diagnostics) != 1 {
		t.Fatal(state)
	}
	err = c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{"mason", "default"}, nil)
	assertCode(t, err, Conflict)
	disk, err := os.ReadFile(path)
	must(t, err)
	if string(disk) != "{invalid secret-runtime-text" {
		t.Fatal("disk overwritten")
	}
	must(t, os.WriteFile(path, original, 0600))
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "default"})
	must(t, s.Close())
}
func assertCode(t *testing.T, err error, code Code) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}
func TestErrors(t *testing.T) {
	opts := fixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	for _, body := range []string{"null", "[]", "{} {}", `{"role":"mason","profile":"other","extra":1}`, `{"role":"mason","role":"reviewer","profile":"other"}`, `{"role":"mason","Role":"reviewer","profile":"other"}`, strings.Repeat(" ", 1<<20) + "{}"} {
		req, err := http.NewRequest("PUT", "http://osmia/v1/runtime/profile", strings.NewReader(body))
		must(t, err)
		resp, err := c.http.Do(req)
		must(t, err)
		var v ErrorResponse
		must(t, json.NewDecoder(resp.Body).Decode(&v))
		resp.Body.Close()
		if v.Error.Code != Malformed || resp.StatusCode != 400 {
			t.Fatal(v, resp.StatusCode)
		}
	}
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{"missing", "secret"}, nil), Validation)
	assertCode(t, c.Do(ctx, "POST", Prefix+"/reload", nil, nil), Unsupported)
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/config/listen", nil, nil), RestartRequired)
	// A non-regular runtime file is a storage failure, not a validation failure.
	must(t, os.Mkdir(filepath.Join(opts.Config.Root, "runtime.json"), 0700))
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{"mason", "other"}, nil), Internal)
	must(t, s.Close())
	assertCode(t, c.Do(ctx, "GET", Prefix+"/health", nil, nil), Unavailable)
}
func TestConcurrentAcknowledgementsSurvive(t *testing.T) {
	opts := fixture(t)
	for i := 0; i < 24; i++ {
		opts.Workstreams = append(opts.Workstreams, config.WorkstreamID(fmt.Sprintf("w_%032x", i)))
	}
	s, c := start(t, opts)
	var wg sync.WaitGroup
	errs := make(chan error, 48)
	for _, id := range opts.Workstreams[1:] {
		wg.Add(1)
		go func(id config.WorkstreamID) {
			defer wg.Done()
			var result MutationResponse
			err := c.Do(context.Background(), "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: id}, Mode: "soft", Source: "operator"}, &result)
			if err != nil {
				errs <- err
				return
			}
			if !result.Applied {
				errs <- fmt.Errorf("not acknowledged")
			}
			_, err = c.Runtime(context.Background())
			if err != nil {
				errs <- err
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	before, err := c.Runtime(context.Background())
	must(t, err)
	if len(before.Effective.Pauses) != 24 {
		t.Fatal(before)
	}
	must(t, s.Close())
	_, c2 := start(t, opts)
	after, err := c2.Runtime(context.Background())
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("acknowledged changes lost")
	}
}
func TestCancellationClosesInflightAndReleasesRoot(t *testing.T) {
	opts := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, opts)
	must(t, err)
	defer s.Close()
	conn, err := net.Dial("unix", s.Socket())
	must(t, err)
	defer conn.Close()
	_, err = io.WriteString(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: osmia\r\nContent-Length: 10000\r\n\r\n{\"role\":")
	must(t, err)
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hung on unfinished request")
	}
	must(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = io.ReadAll(conn)
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("connection remained open")
	}
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "runtime.json")); !os.IsNotExist(err) {
		t.Fatal("partial request persisted")
	}
	_, c := start(t, opts)
	_, err = c.Health(context.Background())
	must(t, err)
}

func TestShutdownDrainsAcceptedMutation(t *testing.T) {
	opts := fixture(t)
	s, _ := start(t, opts)
	conn, err := net.Dial("unix", s.Socket())
	must(t, err)
	defer conn.Close()
	body := `{"role":"mason","profile":"other"}`
	_, err = fmt.Fprintf(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: osmia\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(body))
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
	_, c := start(t, opts)
	state, err := c.Runtime(context.Background())
	must(t, err)
	if state.Effective.Profiles["mason"] != "other" {
		t.Fatal("drained mutation lost")
	}
}
func TestFailedStartupAndConfiguredSocket(t *testing.T) {
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "runtime.json")
	must(t, os.WriteFile(path, []byte("not json"), 0600))
	if s, err := Start(context.Background(), opts); err == nil {
		s.Close()
		t.Fatal("invalid startup accepted")
	}
	must(t, os.Remove(path))
	cfgPath := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(cfgPath)
	must(t, err)
	must(t, os.WriteFile(cfgPath, append(data, []byte("\n[listen]\nsocket = 'custom.sock'\n")...), 0600))
	s, c := start(t, opts)
	if filepath.Base(s.Socket()) != "custom.sock" {
		t.Fatal(s.Socket())
	}
	_, err = c.Health(context.Background())
	must(t, err)
	// Cleanup must leave a replacement artifact alone.
	must(t, os.Remove(s.Socket()))
	must(t, os.WriteFile(s.Socket(), []byte("replacement"), 0600))
	must(t, s.Close())
	data, err = os.ReadFile(s.Socket())
	must(t, err)
	if string(data) != "replacement" {
		t.Fatal("removed foreign artifact")
	}
}

func TestStaleDiagnosticsDoNotExposeDiskValues(t *testing.T) {
	opts := fixture(t)
	must(t, os.WriteFile(filepath.Join(opts.Config.Root, "runtime.json"), []byte(`{"version":1,"profiles":{"secret-role":"secret-profile"}}`), 0600))
	_, c := start(t, opts)
	state, err := c.Runtime(context.Background())
	must(t, err)
	if len(state.Diagnostics) != 1 || state.Diagnostics[0].Field != "profiles" {
		t.Fatal(state)
	}
	data, err := json.Marshal(state)
	must(t, err)
	if bytes.Contains(data, []byte("secret-")) {
		t.Fatal("stale disk values leaked")
	}
}
