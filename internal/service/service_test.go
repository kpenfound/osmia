package service

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const project config.ProjectID = "p_0123456789abcdef0123456789abcdef"
const stream config.WorkstreamID = "w_0123456789abcdef0123456789abcdef"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// traceQuery runs query against the trace database in directory through a
// read-only connection of its own, which needs no lock and so reads beside
// any handle the service holds.
func traceQuery(t *testing.T, directory, query string, args ...any) [][]byte {
	t.Helper()
	out, err := queryTrace(directory, query, args...)
	must(t, err)
	return out
}

// queryTrace is traceQuery returning its error.
func queryTrace(directory, query string, args ...any) ([][]byte, error) {
	db, err := sql.Open("sqlite", "file:"+trace.TraceDatabase(directory)+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// traceExec runs statement against the trace database in directory, which no
// handle may hold open, to stage damage a test then reads back.
func traceExec(t *testing.T, directory, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+trace.TraceDatabase(directory)+"?_pragma=busy_timeout(5000)")
	must(t, err)
	defer db.Close()
	_, err = db.Exec(statement, args...)
	must(t, err)
}

// workstreamTraceDirectory is stream's own trace directory.
func workstreamTraceDirectory(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID) string {
	t.Helper()
	dir, err := root.WorkstreamTrace(project, stream)
	must(t, err)
	return dir
}

// workstreamRecords returns the bodies of the records stream's trace holds at
// its logical path name, in the order they were recorded.
func workstreamRecords(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, name string) [][]byte {
	t.Helper()
	return traceQuery(t, workstreamTraceDirectory(t, root, project, stream), "SELECT body FROM records WHERE path = ? ORDER BY seq", "workstreams/"+string(stream)+"/"+name)
}

// latestDocumentIn returns the content of the latest revision of the
// document at path in the trace in directory, whose documents are recorded
// at log. A document with no revision is fs.ErrNotExist.
func latestDocumentIn(directory, log, path string) ([]byte, error) {
	bodies, err := queryTrace(directory, "SELECT body FROM records WHERE path = ? ORDER BY seq", log)
	if err != nil {
		return nil, err
	}
	var latest *trace.Document
	for _, body := range bodies {
		var d trace.Document
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		if d.Path == path {
			latest = &d
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return []byte(latest.Content), nil
}

// workstreamDocument returns the content of the latest revision of the
// document at path in stream's trace.
func workstreamDocument(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, path string) ([]byte, error) {
	t.Helper()
	return latestDocumentIn(workstreamTraceDirectory(t, root, project, stream), "workstreams/"+string(stream)+"/documents.jsonl", path)
}

// projectDocument returns the content of the latest revision of the project
// document at path.
func projectDocument(t *testing.T, root config.Root, project config.ProjectID, path string) ([]byte, error) {
	t.Helper()
	dir, err := root.ProjectTrace(project)
	must(t, err)
	return latestDocumentIn(dir, "documents.jsonl", path)
}

// traceCommits returns how many commits the trace in directory holds.
func traceCommits(t *testing.T, directory string) int {
	t.Helper()
	rows := traceQuery(t, directory, "SELECT CAST(count(*) AS TEXT) FROM commits")
	n, err := strconv.Atoi(string(rows[0]))
	must(t, err)
	return n
}

// writeCharter records content as the project's next charter revision
// through the API, as the owner does, and returns that revision.
func writeCharter(t *testing.T, c *Client, project config.ProjectID, content string) trace.Document {
	t.Helper()
	ctx := context.Background()
	current, err := c.Charter(ctx, project)
	must(t, err)
	doc, err := c.EditCharter(ctx, project, CharterEdit{Revision: current.Revision, Content: content})
	must(t, err)
	return doc
}

// editDraft records spec and plan as the owner's edit of stream's draft
// through the API. An empty argument keeps that document as it is.
func editDraft(t *testing.T, c *Client, stream config.WorkstreamID, spec, plan string) map[string]trace.Document {
	t.Helper()
	ctx := context.Background()
	docs, err := c.Documents(ctx, stream)
	must(t, err)
	if spec == "" {
		spec = docs["spec"].Content
	}
	if plan == "" {
		plan = docs["plan"].Content
	}
	edited, err := c.EditDraft(ctx, stream, DraftEdit{SpecRevision: docs["spec"].Revision, PlanRevision: docs["plan"].Revision, Spec: spec, Plan: plan})
	must(t, err)
	return edited
}

// awaitAcknowledged polls fetch until it returns operations and the reconcile
// controller has acknowledged every one of them. An operation's result and its
// acknowledgement are committed after the state its Apply commits, so a test
// that reached an operation through an awaited state must wait here before it
// asserts on the operation's result, acknowledgement or history.
func awaitAcknowledged(t *testing.T, fetch func(*testing.T) []trace.OperationRecord) []trace.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		ops := fetch(t)
		acknowledged := len(ops) > 0
		for _, o := range ops {
			if !o.Acknowledged {
				acknowledged = false
			}
		}
		if acknowledged {
			return ops
		}
		if time.Now().After(deadline) {
			t.Fatalf("operations were not acknowledged: %+v", ops)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func fixture(t *testing.T) Options {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	return fixtureAt(t, home)
}

// fixtureAt writes a minimal root and project configuration under home. Its
// workstreams are created on Git worktrees whether or not jj is installed.
func fixtureAt(t *testing.T, home string) Options {
	t.Helper()
	root := filepath.Join(home, "root")
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(project)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version = 1
active_projects = [%q]
workspaces = "git"
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
	var result MutationResponse
	must(t, c.Do(context.Background(), method, Prefix+"/runtime/"+kind, input, &result))
	if !result.Applied {
		t.Fatal("not acknowledged")
	}
}
func TestRoundTrip(t *testing.T) {
	t.Parallel()
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
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "hard", Source: "owner"})
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{stream}})
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	before, err := c.Runtime(ctx)
	must(t, err)
	if len(before.Effective.Pauses) != 1 || len(before.Effective.Priorities) != 1 || before.Effective.Profiles["mason"] != "other" {
		t.Fatal(before)
	}
	if len(before.Profiles) != len(s.cfg.Roles) || before.Profiles["mason"] != (EffectiveProfile{Name: "other", Source: "owner_override"}) || before.Profiles["reviewer"] != (EffectiveProfile{Name: "default", Source: "configuration"}) {
		t.Fatalf("effective role profiles: %+v", before.Profiles)
	}
	status, err := c.Statuses(ctx)
	must(t, err)
	if !reflect.DeepEqual(status.Profiles, before.Profiles) {
		t.Fatalf("status role profiles: %+v", status.Profiles)
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
	if after.Profiles["mason"] != (EffectiveProfile{Name: "default", Source: "configuration"}) {
		t.Fatalf("cleared role profile: %+v", after.Profiles)
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
	t.Parallel()
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
	t.Parallel()
	opts := fixture(t)
	path := filepath.Join(opts.Config.Root, "osmia.sock")
	// Bind without listening: a closed listener stays reachable while a
	// child forked by a parallel test still holds its descriptor.
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	must(t, err)
	syscall.CloseOnExec(fd)
	must(t, syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}))
	must(t, syscall.Close(fd))
	_, c := start(t, opts)
	_, err = c.Health(context.Background())
	must(t, err)
}
func TestForeignLiveSocket(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if now.Diagnostics[0].Code != ReloadRequired || now.Digest != cfg.Digest {
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
	t.Parallel()
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
	assertCode(t, c.Do(ctx, "GET", Prefix+"/reload", nil, nil), Unsupported)
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/config/listen", nil, nil), RestartRequired)
	// A non-regular runtime file is a storage failure, not a validation failure.
	must(t, os.Mkdir(filepath.Join(opts.Config.Root, "runtime.json"), 0700))
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{"mason", "other"}, nil), Internal)
	must(t, s.Close())
	assertCode(t, c.Do(ctx, "GET", Prefix+"/health", nil, nil), Unavailable)
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
			err := c.Do(context.Background(), "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: id}, Mode: "soft", Source: "owner"}, &result)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestCloseReleasesTheRootWhileItsLockIsShared(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, err := Start(context.Background(), opts)
	must(t, err)
	// A duplicate descriptor shares the lock the way a child process forked
	// but not yet executed does.
	dup, err := syscall.Dup(int(s.lock.Fd()))
	must(t, err)
	defer syscall.Close(dup)
	must(t, s.Close())
	start(t, opts)
}

// recordSpec records content as the owner's next revision of stream's spec
// directly in its trace, whatever the workstream's state.
func recordSpec(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, content string) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	var latest *trace.Document
	for i := range docs {
		if docs[i].Path == "spec.md" {
			latest = &docs[i]
		}
	}
	if latest == nil {
		t.Fatalf("workstream %s records no spec", stream)
	}
	next := *latest
	next.Revision++
	next.Content, next.At, next.Actor, next.Cause, next.Depth = content, latest.At.Add(time.Second), trace.Actor{Kind: "owner", ID: "local"}, "owner-edit", 0
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{next}))
}

// documentCommit returns the commit that recorded the latest revision of the
// document at path in stream's trace.
func documentCommit(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, path string) string {
	t.Helper()
	rows := traceQuery(t, workstreamTraceDirectory(t, root, project, stream), "SELECT CAST(commit_seq AS TEXT) FROM records WHERE path = ? AND schema = 'osmia.trace.document' AND json_extract(body, '$.path') = ? ORDER BY seq DESC LIMIT 1", "workstreams/"+string(stream)+"/documents.jsonl", path)
	if len(rows) != 1 {
		t.Fatalf("workstream %s records no %s", stream, path)
	}
	return string(rows[0])
}

// traceFile returns the trace file name of the trace in directory, such as
// a role's notes, and whether it exists.
func traceFile(t *testing.T, directory, name string) ([]byte, bool) {
	t.Helper()
	rows := traceQuery(t, directory, "SELECT content FROM files WHERE path = ?", name)
	if len(rows) == 0 {
		return nil, false
	}
	return rows[0], true
}

// recordCharter records content as the owner's next charter revision through
// repo, a trace the test holds itself.
func recordCharter(t *testing.T, repo *trace.Repository, content string) {
	t.Helper()
	ctx := context.Background()
	current, err := repo.Charter(ctx)
	must(t, err)
	_, err = repo.EditCharter(ctx, current.Revision, content, current.At.Add(time.Second))
	must(t, err)
}

// committedPaths returns, for each commit of the trace in directory after
// the first after commits, the record paths and document paths it wrote.
func committedPaths(t *testing.T, directory string, after int) []string {
	t.Helper()
	var out []string
	for _, seq := range traceQuery(t, directory, "SELECT CAST(seq AS TEXT) FROM commits WHERE seq > ? ORDER BY seq", after) {
		paths := traceQuery(t, directory, "SELECT path || ' ' || coalesce(json_extract(body, '$.path'), '') FROM records WHERE commit_seq = ? ORDER BY seq", string(seq))
		out = append(out, string(bytes.Join(paths, []byte("\n"))))
	}
	return out
}
