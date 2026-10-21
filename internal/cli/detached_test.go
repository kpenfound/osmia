package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/service"
)

// testIssuesBaseURLEnv names the environment variable
// TestDetachedServiceLogsGitHubActionsOnRealStandardError sets so the
// detached child subprocess it launches fetches issues from a fake server
// instead of api.github.com. Production never sets it.
const testIssuesBaseURLEnv = "OSMIA_TEST_ISSUES_BASE_URL"

func TestDetachedProcess(t *testing.T) {
	if os.Getenv(readinessEnv) != "3" {
		return
	}
	// TestMain installs fake execution boundaries. This process has no projects.
	if base := os.Getenv(testIssuesBaseURLEnv); base != "" {
		baseServiceOptions = func(root config.Root, logger *slog.Logger) service.Options {
			return service.Options{Config: root.Options(""), Build: build(), Logger: logger,
				Issues: issues.GitHub{BaseURL: base, HTTP: http.DefaultClient}}
		}
	}
	root := os.Args[len(os.Args)-1]
	os.Exit(Run(context.Background(), []string{"serve", "--root", root}, strings.NewReader(""), os.Stdout, os.Stderr))
}

func TestDetachedServeReadinessDuplicateOwnerAndStop(t *testing.T) {
	root, err := os.MkdirTemp("", "od-")
	must(t, err)
	defer os.RemoveAll(root)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	original := detachedCommand
	detachedCommand = func(root string) (*exec.Cmd, error) {
		return exec.Command(os.Args[0], "-test.run=^TestDetachedProcess$", "--", root), nil
	}
	defer func() { detachedCommand = original }()
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	defer c.Do(context.Background(), "POST", wire.Prefix+"/stop", nil, nil)
	code, out, diag := invoke(t, root, "serve", "--detach")
	if code != 0 || diag != "" || !strings.Contains(out, "Osmia is ready") {
		t.Fatalf("start %d %s %s", code, out, diag)
	}
	health, err := c.Health(context.Background())
	must(t, err)
	if !health.Ready {
		t.Fatalf("not ready: %+v", health)
	}
	code, _, _ = invoke(t, root, "serve", "--detach")
	if code != 6 {
		t.Fatalf("duplicate service started: %d", code)
	}
	info, err := os.Stat(filepath.Join(root, "service.log"))
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions %s", info.Mode())
	}
	var output, diagnostics bytes.Buffer
	code = Run(context.Background(), []string{"stop", "--root", root}, strings.NewReader(""), &output, &diagnostics)
	if code != 0 {
		t.Fatalf("stop %d %s", code, diagnostics.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = c.Health(context.Background())
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A detached default root starts its child without --root so the child reads
// the same XDG config file; an explicit root is passed on.
func TestDetachedRootFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "conf"))
	root, err := config.ResolveRoot("", "")
	must(t, err)
	if flag := rootFlag(root); flag != "" {
		t.Fatalf("default root flag %q", flag)
	}
	explicit, err := config.ResolveRoot(filepath.Join(home, "explicit"), "")
	must(t, err)
	if flag := rootFlag(explicit); flag != explicit.String() {
		t.Fatalf("explicit root flag %q", flag)
	}
}

// TestServeFailureNamesARuntimeFailure shows a service that stopped after it
// started naming its failure, redacted and on one line, while a startup
// failure still points at doctor without echoing its error.
func TestServeFailureNamesARuntimeFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	at := time.Date(2026, 10, 3, 19, 58, 13, 0, time.UTC)
	failure := errors.Join(
		errors.New("project p_1: workstream w_1: push https://x-access-token:hunter2@github.com/acme/widgets.git refused"),
		errors.New("token ghp_secret\x1b[31m rejected"))
	var out strings.Builder
	reportServeFailure(&out, stoppedError{failure}, at)
	want := "2026-10-03T19:58:13Z service stopped after a failure: project p_1: workstream w_1: push https://[redacted]@github.com/acme/widgets.git refused; token [redacted] [31m rejected\n" +
		"fix the cause above, then start the service again\n"
	if out.String() != want {
		t.Fatalf("runtime failure:\n got %q\nwant %q", out.String(), want)
	}
	out.Reset()
	reportServeFailure(&out, errors.New("config.toml: token = \"ghp_secret\""), at)
	if got := out.String(); strings.Contains(got, "ghp_secret") || strings.Contains(got, "config.toml") || !strings.Contains(got, "osmia doctor") {
		t.Fatalf("startup failure: %q", got)
	}
	out.Reset()
	reportServeFailure(&out, stoppedError{errors.New(strings.Repeat("é", maxFailureText))}, at)
	if line, _, _ := strings.Cut(out.String(), "\n"); !strings.HasSuffix(line, "…") || !utf8.ValidString(line) || len(line) > maxFailureText+100 {
		t.Fatalf("an unbounded failure: %d bytes", len(line))
	}
}

// TestLogServeFailureRecordsAStructuredErrorRecord shows that the service
// process itself, whether a foreground osmia serve or the detached child
// whose stderr is service.log, reports its own failure as one error-level
// structured record, at startup and after it started, with the same
// redaction and guidance reportServeFailure gives the launcher.
func TestLogServeFailureRecordsAStructuredErrorRecord(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	failure := errors.New("project p_1: workstream w_1: push https://x-access-token:hunter2@github.com/acme/widgets.git refused")
	var buf bytes.Buffer
	logger := newServiceLogger(&buf)
	logServeFailure(logger, stoppedError{failure})

	var record struct {
		Level, Msg, Error, Guidance string
	}
	must(t, json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &record))
	if record.Level != "ERROR" {
		t.Fatalf("level %q, want ERROR", record.Level)
	}
	if strings.Contains(record.Error, "hunter2") || strings.Contains(record.Error, "ghp_secret") {
		t.Fatalf("record leaked a credential: %+v", record)
	}
	if record.Guidance != "fix the cause above, then start the service again" {
		t.Fatalf("guidance %q", record.Guidance)
	}

	buf.Reset()
	logServeFailure(logger, errors.New("config.toml: token = \"ghp_secret\""))
	text := buf.String()
	if strings.Contains(text, "ghp_secret") || strings.Contains(text, "config.toml") || !strings.Contains(text, "osmia doctor") {
		t.Fatalf("startup failure: %s", text)
	}
}

// TestNewServiceLoggerRedactsKnownSecretsFromEveryWrite shows the writer
// underneath the service's logger redacts known secrets even when a
// record's own fields were not, a final guard besides each record's own
// redaction.
func TestNewServiceLoggerRedactsKnownSecretsFromEveryWrite(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	var buf bytes.Buffer
	logger := newServiceLogger(&buf)
	logger.Info("message", "raw", "token ghp_secret leaked")
	if strings.Contains(buf.String(), "ghp_secret") {
		t.Fatalf("redacting writer let a known secret through: %s", buf.String())
	}
}

// TestForegroundServeFailureIsAStructuredRecordOnStandardError shows a
// foreground osmia serve reporting a startup failure, before the service
// started, as one error-level structured record on its own standard error,
// with the exit code unchanged.
func TestForegroundServeFailureIsAStructuredRecordOnStandardError(t *testing.T) {
	root, err := os.MkdirTemp("", "fg-")
	must(t, err)
	defer os.RemoveAll(root)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	original := detachedCommand
	detachedCommand = func(root string) (*exec.Cmd, error) {
		return exec.Command(os.Args[0], "-test.run=^TestDetachedProcess$", "--", root), nil
	}
	defer func() { detachedCommand = original }()
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	defer c.Do(context.Background(), "POST", wire.Prefix+"/stop", nil, nil)
	code, out, diag := invoke(t, root, "serve", "--detach")
	if code != 0 || diag != "" || !strings.Contains(out, "Osmia is ready") {
		t.Fatalf("start %d %s %s", code, out, diag)
	}

	code, _, diag = invoke(t, root, "serve")
	if code != 6 {
		t.Fatalf("duplicate foreground service started: %d", code)
	}
	var record struct{ Level, Msg string }
	must(t, json.Unmarshal([]byte(strings.TrimRight(diag, "\n")), &record))
	if record.Level != "ERROR" || record.Msg == "" {
		t.Fatalf("standard error is not a structured record: %q", diag)
	}
}

// TestRunServiceRoutesTheStandardLogPackageThroughItsStructuredLoggerAndRestoresGlobalsAfterward
// shows that while runService's own process is the running service,
// embedded code's standard log.Printf calls reach the same structured
// logger as the service's own records, as a JSON record with a time,
// level and message, and that the previous log and slog defaults come back
// once the service stops.
//
// serial: installGlobalLog takes the process-wide standard log package
// output and slog default for as long as the service it serves is
// running; this test drives that through runService and checks both ends
// of the handoff, so it cannot run alongside another test that also serves
// in this process.
func TestRunServiceRoutesTheStandardLogPackageThroughItsStructuredLoggerAndRestoresGlobalsAfterward(t *testing.T) {
	dir, err := os.MkdirTemp("", "rsg-")
	must(t, err)
	defer os.RemoveAll(dir)
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	root, err := config.ResolveRoot(dir, "")
	must(t, err)

	previousDefault := slog.Default()
	previousOutput := log.Writer()
	previousFlags := log.Flags()

	var buf bytes.Buffer
	logger := newServiceLogger(&buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runService(ctx, root, false, io.Discard, logger) }()

	c := service.NewClient(filepath.Join(dir, "osmia.sock"))
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Health(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	log.Printf("embedded dependency output %d", 7)

	// A second service on the same root, started while the first still
	// holds globalLogMu, must not block waiting for it: installGlobalLog
	// installs nothing for the second and the first service's own
	// socket-ownership check refuses it promptly.
	var secondBuf bytes.Buffer
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runService(context.Background(), root, false, io.Discard, newServiceLogger(&secondBuf))
	}()
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "already owned by another Osmia service") {
			t.Fatalf("second runService on the same root: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second runService on the same root did not return promptly")
	}
	if slog.Default() != logger {
		t.Fatal("slog default changed while the first service still runs")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runService: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop")
	}

	if slog.Default() != previousDefault {
		t.Fatal("slog default was not restored after the service stopped")
	}
	if log.Writer() != previousOutput || log.Flags() != previousFlags {
		t.Fatal("standard log package defaults were not restored after the service stopped")
	}

	var sawEmbedded bool
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct{ Time, Level, Msg string }
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a JSON object: %s: %v", line, err)
		}
		if record.Time == "" || record.Level == "" || record.Msg == "" {
			t.Fatalf("record missing time, level or msg: %s", line)
		}
		if strings.Contains(record.Msg, "embedded dependency output 7") {
			sawEmbedded = true
		}
	}
	if !sawEmbedded {
		t.Fatalf("no structured record for the standard log package call:\n%s", buf.String())
	}
}

// TestDetachedServiceLogsGitHubActionsOnRealStandardError starts a detached
// service as a real subprocess, whose own standard error is service.log, and
// has it fetch a handed-in issue from a fake GitHub server. It then reads
// service.log from disk and shows every line is a JSON object with a
// timestamp, level and message, including one GitHub activity record for
// the fetch. Parsing an Options.Logger buffer from within the test process,
// as other tests here do, would miss a stray write straight to this real
// child process's own standard error; reading the file this subprocess
// actually wrote catches that.
func TestDetachedServiceLogsGitHubActionsOnRealStandardError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/issues/3" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"title":"Title","body":"Body"}`)
	}))
	defer fake.Close()
	t.Setenv(testIssuesBaseURLEnv, fake.URL)

	opts, clone := emptyFixture(t)
	root := opts.Config.Root
	original := detachedCommand
	detachedCommand = func(root string) (*exec.Cmd, error) {
		return exec.Command(os.Args[0], "-test.run=^TestDetachedProcess$", "--", root), nil
	}
	defer func() { detachedCommand = original }()
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	defer c.Do(context.Background(), "POST", wire.Prefix+"/stop", nil, nil)
	code, out, diag := invoke(t, root, "serve", "--detach")
	if code != 0 || diag != "" || !strings.Contains(out, "Osmia is ready") {
		t.Fatalf("start %d %s %s", code, out, diag)
	}

	var added wire.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone, "--json")), &added))
	setCharter(t, root, added.Project.ID, "1. Keep changes small.\n")

	text := successful(t, root, "handin", string(added.Project.ID), "https://github.com/owner/repo/issues/3")
	if !strings.Contains(text, "handed in to project") {
		t.Fatalf("handin: %s", text)
	}

	code = Run(context.Background(), []string{"stop", "--root", root}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("stop %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := c.Health(context.Background()); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}

	data, err := os.ReadFile(filepath.Join(root, "service.log"))
	must(t, err)
	var sawGitHub bool
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Time, Level, Msg, Activity, Access, Action, Project string
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a JSON object: %s: %v", line, err)
		}
		if record.Time == "" || record.Level == "" || record.Msg == "" {
			t.Fatalf("line is missing a timestamp, level or message: %s", line)
		}
		if record.Activity == "github" {
			sawGitHub = true
			if record.Access != "read" || record.Action != "issues.fetch" || record.Project != string(added.Project.ID) {
				t.Fatalf("github record: %s", line)
			}
		}
	}
	if !sawGitHub {
		t.Fatalf("service.log has no GitHub activity record:\n%s", data)
	}
}
