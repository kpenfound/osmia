package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func writeCharter(t *testing.T, c *Client, project config.ProjectID, content string) trace.Document {
	t.Helper()
	return intakefixture.WriteCharter(t, c, project, content)
}

func fixture(t *testing.T) Options {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	return fixtureAt(t, home)
}

func fixtureAt(t *testing.T, home string) Options {
	t.Helper()
	return testOptions(fixturekit.ConfigAt(t, home))
}

func start(t *testing.T, opts Options) (*Service, *Client) {
	t.Helper()
	s, err := Start(context.Background(), opts)
	intakefixture.Must(t, err)
	c := NewClient(s.Socket())
	t.Cleanup(func() {
		c.Close()
		if err := s.Close(); err != nil {
			t.Logf("service close: %v", err)
		}
	})
	return s, c
}

func mutation(t *testing.T, c *Client, method, kind string, input any) {
	t.Helper()
	intakefixture.Mutation(t, c, method, kind, input)
}

func TestHealthReportsReadyWithTheBuild(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	_, c := start(t, opts)
	health, err := c.Health(context.Background())
	intakefixture.Must(t, err)
	if !health.Ready || health.Build != opts.Build || health.APIVersion != 1 {
		t.Fatal(health)
	}
}

func TestConfigurationReportsTheProjectRootAndDigest(t *testing.T) {
	t.Parallel()
	s, c := start(t, fixture(t))
	cfg, err := c.Configuration(context.Background())
	intakefixture.Must(t, err)
	if cfg.Effective.Project.ID != intakefixture.Project || cfg.Root != s.cfg.Root.String() || len(cfg.Digest) != 64 || len(cfg.Diagnostics) != 0 || cfg.Project.CharterState != nil {
		t.Fatalf("%+v", cfg)
	}
}

func TestHandInWithoutATraceIsAnInternalError(t *testing.T) {
	t.Parallel()
	_, c := start(t, fixture(t))
	var api *APIError
	if _, err := c.HandIn(context.Background(), HandInRequest{Project: intakefixture.Project}); !errors.As(err, &api) || api.Code != Internal || !strings.Contains(api.Message, "no trace repository") {
		t.Fatalf("hand-in without a trace: %v", err)
	}
}

func TestTheSocketIsReadableByItsOwnerOnly(t *testing.T) {
	t.Parallel()
	s, _ := start(t, fixture(t))
	info, err := os.Stat(s.Socket())
	intakefixture.Must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestClosingTheServiceRemovesItsSocket(t *testing.T) {
	t.Parallel()
	s, _ := start(t, fixture(t))
	intakefixture.Must(t, s.Close())
	if _, err := os.Lstat(s.Socket()); !os.IsNotExist(err) {
		t.Fatal("socket retained", err)
	}
}

func setOwnerOverrides(t *testing.T, c *Client) {
	t.Helper()
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: "owner"})
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: intakefixture.Project, Workstreams: []config.WorkstreamID{intakefixture.Stream}})
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"})
}

func TestOwnerOverridesAreReportedByRuntime(t *testing.T) {
	t.Parallel()
	s, c := start(t, fixture(t))
	setOwnerOverrides(t, c)
	before, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	if len(before.Effective.Pauses) != 1 || len(before.Effective.Priorities) != 1 || before.Effective.Profiles["mason"] != "other" {
		t.Fatal(before)
	}
	if len(before.Profiles) != len(s.cfg.Roles) || before.Profiles["mason"] != (EffectiveProfile{Name: "other", Source: "owner_override"}) || before.Profiles["reviewer"] != (EffectiveProfile{Name: "default", Source: "configuration"}) {
		t.Fatalf("effective role profiles: %+v", before.Profiles)
	}
}

func TestStatusReportsTheEffectiveRoleProfiles(t *testing.T) {
	t.Parallel()
	_, c := start(t, fixture(t))
	setOwnerOverrides(t, c)
	before, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	status, err := c.Statuses(context.Background())
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(status.Profiles, before.Profiles) {
		t.Fatalf("status role profiles: %+v", status.Profiles)
	}
}

func TestOwnerOverridesSurviveARestart(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, c := start(t, opts)
	setOwnerOverrides(t, c)
	before, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	intakefixture.Must(t, s.Close())
	_, c2 := start(t, opts)
	after, err := c2.Runtime(context.Background())
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal(before, after)
	}
}

func TestClearingOwnerOverridesRestoresTheConfiguredDefaults(t *testing.T) {
	t.Parallel()
	_, c := start(t, fixture(t))
	setOwnerOverrides(t, c)
	mutation(t, c, "DELETE", "pause", ClearPauseRequest{Scope: "factory"})
	mutation(t, c, "DELETE", "priority", ClearPriorityRequest{Project: intakefixture.Project})
	mutation(t, c, "DELETE", "profile", ClearProfileRequest{Role: "mason"})
	after, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	if len(after.Effective.Pauses) != 0 || len(after.Effective.Priorities) != 0 || after.Effective.Profiles["mason"] != "default" {
		t.Fatal(after)
	}
	if after.Profiles["mason"] != (EffectiveProfile{Name: "default", Source: "configuration"}) {
		t.Fatalf("cleared role profile: %+v", after.Profiles)
	}
}

func TestClearedOwnerOverridesStayClearedAfterARestart(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, c := start(t, opts)
	setOwnerOverrides(t, c)
	mutation(t, c, "DELETE", "pause", ClearPauseRequest{Scope: "factory"})
	mutation(t, c, "DELETE", "priority", ClearPriorityRequest{Project: intakefixture.Project})
	mutation(t, c, "DELETE", "profile", ClearProfileRequest{Role: "mason"})
	after, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	intakefixture.Must(t, s.Close())
	_, c2 := start(t, opts)
	cleared, err := c2.Runtime(context.Background())
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(after, cleared) {
		t.Fatal("cleared overrides returned after restart")
	}
}

func TestRootContention(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, c := start(t, opts)
	before, err := os.Lstat(s.Socket())
	intakefixture.Must(t, err)
	alias := filepath.Join(filepath.Dir(opts.Config.Root), "alias")
	intakefixture.Must(t, os.Symlink(opts.Config.Root, alias))
	opts.Config.Root = alias
	if second, err := Start(context.Background(), opts); err == nil {
		second.Close()
		t.Fatal("second owner accepted")
	} else if !strings.Contains(err.Error(), "already owned") {
		t.Fatal(err)
	}
	after, err := os.Lstat(s.Socket())
	intakefixture.Must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("socket replaced")
	}
	_, err = c.Health(context.Background())
	intakefixture.Must(t, err)
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"})
}

func TestStaleSocket(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "osmia.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	intakefixture.Must(t, err)
	syscall.CloseOnExec(fd)
	intakefixture.Must(t, syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}))
	intakefixture.Must(t, syscall.Close(fd))
	_, c := start(t, opts)
	_, err = c.Health(context.Background())
	intakefixture.Must(t, err)
}

func TestForeignLiveSocket(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "osmia.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	intakefixture.Must(t, err)
	defer l.Close()
	before, err := os.Lstat(path)
	intakefixture.Must(t, err)
	if s, err := Start(context.Background(), opts); err == nil {
		s.Close()
		t.Fatal("live socket replaced")
	}
	after, err := os.Lstat(path)
	intakefixture.Must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("live socket disturbed")
	}
	conn, err := net.Dial("unix", path)
	intakefixture.Must(t, err)
	conn.Close()
}

func TestSocketAndLockArtifacts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"file", "symlink", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			opts := fixture(t)
			path := filepath.Join(opts.Config.Root, "osmia.sock")
			if kind == "lock-symlink" {
				path = filepath.Join(opts.Config.Root, ".service.lock")
			}
			if kind == "file" {
				intakefixture.Must(t, os.WriteFile(path, []byte("keep"), 0600))
			} else {
				intakefixture.Must(t, os.Symlink(filepath.Join(opts.Config.Root, "config.toml"), path))
			}
			before, err := os.Lstat(path)
			intakefixture.Must(t, err)
			if s, err := Start(context.Background(), opts); err == nil {
				s.Close()
				t.Fatal("unsafe artifact accepted")
			}
			after, err := os.Lstat(path)
			intakefixture.Must(t, err)
			if !os.SameFile(before, after) {
				t.Fatal("artifact changed")
			}
		})
	}
}

func TestInvalidDiskRetainsView(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"})
	before, err := c.Runtime(ctx)
	intakefixture.Must(t, err)
	cfg, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	path := filepath.Join(opts.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(path, []byte("credential = 'secret-parser-text'\n[broken"), 0600))
	now, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	if now.Digest != cfg.Digest || !reflect.DeepEqual(now.Effective, cfg.Effective) || len(now.Diagnostics) != 1 {
		t.Fatal(now)
	}
	encoded, _ := json.Marshal(now)
	if bytes.Contains(encoded, []byte("secret-parser-text")) {
		t.Fatal("parser text leaked")
	}
	intakefixture.Must(t, os.WriteFile(path, append(original, []byte("\n[capacity]\nmasons = 7\n")...), 0600))
	now, err = c.Configuration(ctx)
	intakefixture.Must(t, err)
	if now.Diagnostics[0].Code != ReloadRequired || now.Digest != cfg.Digest {
		t.Fatal(now)
	}
	path = filepath.Join(opts.Config.Root, "runtime.json")
	original, err = os.ReadFile(path)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(path, []byte("{invalid secret-runtime-text"), 0600))
	state, err := c.Runtime(ctx)
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(state.Effective, before.Effective) || len(state.Diagnostics) != 1 {
		t.Fatal(state)
	}
	err = c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{Role: "mason", Profile: "default"}, nil)
	fixturekit.AssertCode(t, err, Conflict)
	disk, err := os.ReadFile(path)
	intakefixture.Must(t, err)
	if string(disk) != "{invalid secret-runtime-text" {
		t.Fatal("disk overwritten")
	}
	intakefixture.Must(t, os.WriteFile(path, original, 0600))
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "default"})
	intakefixture.Must(t, s.Close())
}

func TestErrors(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	for _, body := range []string{"null", "[]", "{} {}", `{"role":"mason","profile":"other","extra":1}`, `{"role":"mason","role":"reviewer","profile":"other"}`, `{"role":"mason","Role":"reviewer","profile":"other"}`, strings.Repeat(" ", 1<<20) + "{}"} {
		req, err := http.NewRequest("PUT", "http://osmia/v1/runtime/profile", strings.NewReader(body))
		intakefixture.Must(t, err)
		resp, err := c.http.Do(req)
		intakefixture.Must(t, err)
		var v ErrorResponse
		intakefixture.Must(t, json.NewDecoder(resp.Body).Decode(&v))
		resp.Body.Close()
		if v.Error.Code != Malformed || resp.StatusCode != 400 {
			t.Fatal(v, resp.StatusCode)
		}
	}
	fixturekit.AssertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{Role: "missing", Profile: "secret"}, nil), Validation)
	fixturekit.AssertCode(t, c.Do(ctx, "GET", Prefix+"/reload", nil, nil), Unsupported)
	fixturekit.AssertCode(t, c.Do(ctx, "PUT", Prefix+"/config/listen", nil, nil), RestartRequired)
	intakefixture.Must(t, os.Mkdir(filepath.Join(opts.Config.Root, "runtime.json"), 0700))
	fixturekit.AssertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{Role: "mason", Profile: "other"}, nil), Internal)
	intakefixture.Must(t, s.Close())
	fixturekit.AssertCode(t, c.Do(ctx, "GET", Prefix+"/health", nil, nil), Unavailable)
}

func TestConcurrentAcknowledgementsSurvive(t *testing.T) {
	t.Parallel()
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
			err := c.Do(context.Background(), "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: intakefixture.Project, Workstream: id}, Mode: "soft", Source: "owner"}, &result)
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
	intakefixture.Must(t, err)
	if len(before.Effective.Pauses) != 24 {
		t.Fatal(before)
	}
	intakefixture.Must(t, s.Close())
	_, c2 := start(t, opts)
	after, err := c2.Runtime(context.Background())
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("acknowledged changes lost")
	}
}

func TestCancellationClosesInflightAndReleasesRoot(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	defer s.Close()
	conn, err := net.Dial("unix", s.Socket())
	intakefixture.Must(t, err)
	defer conn.Close()
	_, err = io.WriteString(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: osmia\r\nContent-Length: 10000\r\n\r\n{\"role\":")
	intakefixture.Must(t, err)
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		intakefixture.Must(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hung on unfinished request")
	}
	intakefixture.Must(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = io.ReadAll(conn)
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("connection remained open")
	}
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "runtime.json")); !os.IsNotExist(err) {
		t.Fatal("partial request persisted")
	}
	_, c := start(t, opts)
	_, err = c.Health(context.Background())
	intakefixture.Must(t, err)
}

func TestShutdownDrainsAcceptedMutation(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, _ := start(t, opts)
	conn, err := net.Dial("unix", s.Socket())
	intakefixture.Must(t, err)
	defer conn.Close()
	body := `{"role":"mason","profile":"other"}`
	_, err = fmt.Fprintf(conn, "PUT /v1/runtime/profile HTTP/1.1\r\nHost: osmia\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(body))
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
	_, c := start(t, opts)
	state, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	if state.Effective.Profiles["mason"] != "other" {
		t.Fatal("drained mutation lost")
	}
}

func TestFailedStartupAndConfiguredSocket(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "runtime.json")
	intakefixture.Must(t, os.WriteFile(path, []byte("not json"), 0600))
	if s, err := Start(context.Background(), opts); err == nil {
		s.Close()
		t.Fatal("invalid startup accepted")
	}
	intakefixture.Must(t, os.Remove(path))
	cfgPath := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(cfgPath)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(cfgPath, append(data, []byte("\n[listen]\nsocket = 'custom.sock'\n")...), 0600))
	s, c := start(t, opts)
	if filepath.Base(s.Socket()) != "custom.sock" {
		t.Fatal(s.Socket())
	}
	_, err = c.Health(context.Background())
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.Remove(s.Socket()))
	intakefixture.Must(t, os.WriteFile(s.Socket(), []byte("replacement"), 0600))
	intakefixture.Must(t, s.Close())
	data, err = os.ReadFile(s.Socket())
	intakefixture.Must(t, err)
	if string(data) != "replacement" {
		t.Fatal("removed foreign artifact")
	}
}

func TestStaleDiagnosticsDoNotExposeDiskValues(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	intakefixture.Must(t, os.WriteFile(filepath.Join(opts.Config.Root, "runtime.json"), []byte(`{"version":1,"profiles":{"secret-role":"secret-profile"}}`), 0600))
	_, c := start(t, opts)
	state, err := c.Runtime(context.Background())
	intakefixture.Must(t, err)
	if len(state.Diagnostics) != 1 || state.Diagnostics[0].Field != "profiles" {
		t.Fatal(state)
	}
	data, err := json.Marshal(state)
	intakefixture.Must(t, err)
	if bytes.Contains(data, []byte("secret-")) {
		t.Fatal("stale disk values leaked")
	}
}

func TestCloseReleasesTheRootWhileItsLockIsShared(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, err := Start(context.Background(), opts)
	intakefixture.Must(t, err)
	dup, err := syscall.Dup(int(s.lock.Fd()))
	intakefixture.Must(t, err)
	defer syscall.Close(dup)
	intakefixture.Must(t, s.Close())
	start(t, opts)
}
