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
	"github.com/kpenfound/osmia/internal/engine"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/service"
)

const testIssuesBaseURLEnv = "OSMIA_TEST_ISSUES_BASE_URL"

func TestDetachedProcess(t *testing.T) {
	if os.Getenv(readinessEnv) != "3" {
		return
	}
	if base := os.Getenv(testIssuesBaseURLEnv); base != "" {
		baseServiceOptions = func(root config.Root, logger *slog.Logger) service.Options {
			return service.Options{Config: root.Options(""), Build: build(), Logger: logger,
				Issues: issues.GitHub{BaseURL: base, HTTP: http.DefaultClient}}
		}
	}
	root := os.Args[len(os.Args)-1]
	os.Exit(Run(context.Background(), []string{"serve", "--root", root}, strings.NewReader(""), os.Stdout, os.Stderr))
}

const minimalConfig = "version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"

type detachedService struct {
	root   string
	client *service.Client
	out    string
}

func startDetachedService(t *testing.T) detachedService {
	t.Helper()
	root, err := os.MkdirTemp("", "od-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(minimalConfig), 0600))
	original := detachedCommand
	detachedCommand = func(root string) (*exec.Cmd, error) {
		return exec.Command(os.Args[0], "-test.run=^TestDetachedProcess$", "--", root), nil
	}
	t.Cleanup(func() { detachedCommand = original })
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	t.Cleanup(c.Close)
	t.Cleanup(func() { c.Do(context.Background(), "POST", wire.Prefix+"/stop", nil, nil) })
	code, out, diag := invoke(t, root, "serve", "--detach")
	if code != 0 || diag != "" {
		t.Fatalf("start %d %s %s", code, out, diag)
	}
	return detachedService{root: root, client: c, out: out}
}

func TestDetachedServeReportsReadinessOnceTheServiceAnswers(t *testing.T) {
	d := startDetachedService(t)
	if !strings.Contains(d.out, "Osmia is ready") {
		t.Fatalf("start output %q", d.out)
	}
	health, err := d.client.Health(context.Background())
	must(t, err)
	if !health.Ready {
		t.Fatalf("not ready: %+v", health)
	}
}

func TestDetachedServeRefusesASecondServiceOnTheRoot(t *testing.T) {
	d := startDetachedService(t)
	if code, _, _ := invoke(t, d.root, "serve", "--detach"); code != 6 {
		t.Fatalf("duplicate service started: %d", code)
	}
}

func TestDetachedServiceLogIsReadableOnlyByTheOwner(t *testing.T) {
	d := startDetachedService(t)
	info, err := os.Stat(filepath.Join(d.root, "service.log"))
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions %s", info.Mode())
	}
}

func TestStopShutsTheDetachedServiceDown(t *testing.T) {
	d := startDetachedService(t)
	var output, diagnostics bytes.Buffer
	if code := Run(context.Background(), []string{"stop", "--root", d.root}, strings.NewReader(""), &output, &diagnostics); code != 0 {
		t.Fatalf("stop %d %s", code, diagnostics.String())
	}
	awaitHealth(t, d.client, false, 10*time.Second, nil)
}

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
}

func TestServeFailureAtStartupHidesTheFailureAndPointsToDoctor(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	var out strings.Builder
	reportServeFailure(&out, errors.New("config.toml: token = \"ghp_secret\""), time.Date(2026, 10, 3, 19, 58, 13, 0, time.UTC))
	if got := out.String(); strings.Contains(got, "ghp_secret") || strings.Contains(got, "config.toml") || !strings.Contains(got, "osmia doctor") {
		t.Fatalf("startup failure: %q", got)
	}
}

func TestServeFailureTruncatesAnUnboundedFailure(t *testing.T) {
	var out strings.Builder
	reportServeFailure(&out, stoppedError{errors.New(strings.Repeat("é", maxFailureText))}, time.Date(2026, 10, 3, 19, 58, 13, 0, time.UTC))
	if line, _, _ := strings.Cut(out.String(), "\n"); !strings.HasSuffix(line, "…") || !utf8.ValidString(line) || len(line) > maxFailureText+100 {
		t.Fatalf("an unbounded failure: %d bytes", len(line))
	}
}

func TestLogServeFailureRecordsARuntimeFailureAsAnErrorWithGuidance(t *testing.T) {
	var buf bytes.Buffer
	logServeFailure(newServiceLogger(&buf), stoppedError{errors.New("project p_1: workstream w_1: push refused")})
	var record struct {
		Level, Msg, Error, Guidance string
	}
	must(t, json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &record))
	if record.Level != "ERROR" {
		t.Fatalf("level %q, want ERROR", record.Level)
	}
	if record.Guidance != "fix the cause above, then start the service again" {
		t.Fatalf("guidance %q", record.Guidance)
	}
}

func TestLogServeFailureRedactsCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	var buf bytes.Buffer
	logServeFailure(newServiceLogger(&buf), stoppedError{errors.New("project p_1: workstream w_1: push https://x-access-token:hunter2@github.com/acme/widgets.git refused with ghp_secret")})
	var record struct{ Error string }
	must(t, json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &record))
	if strings.Contains(record.Error, "hunter2") || strings.Contains(record.Error, "ghp_secret") {
		t.Fatalf("record leaked a credential: %+v", record)
	}
}

func TestLogServeFailureAtStartupHidesTheFailureAndPointsToDoctor(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	var buf bytes.Buffer
	logServeFailure(newServiceLogger(&buf), errors.New("config.toml: token = \"ghp_secret\""))
	text := buf.String()
	if strings.Contains(text, "ghp_secret") || strings.Contains(text, "config.toml") || !strings.Contains(text, "osmia doctor") {
		t.Fatalf("startup failure: %s", text)
	}
}

func TestNewServiceLoggerRedactsKnownSecretsFromEveryWrite(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	var buf bytes.Buffer
	logger := newServiceLogger(&buf)
	logger.Info("message", "raw", "token ghp_secret leaked")
	if strings.Contains(buf.String(), "ghp_secret") {
		t.Fatalf("redacting writer let a known secret through: %s", buf.String())
	}
}

func TestForegroundServeFailureIsAStructuredRecordOnStandardError(t *testing.T) {
	d := startDetachedService(t)
	code, _, diag := invoke(t, d.root, "serve")
	if code != 6 {
		t.Fatalf("duplicate foreground service started: %d", code)
	}
	var record struct{ Level, Msg string }
	must(t, json.Unmarshal([]byte(strings.TrimRight(diag, "\n")), &record))
	if record.Level != "ERROR" || record.Msg == "" {
		t.Fatalf("standard error is not a structured record: %q", diag)
	}
}

type runningService struct {
	root    config.Root
	logger  *slog.Logger
	logs    *bytes.Buffer
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func startRunService(t *testing.T) *runningService {
	t.Helper()
	dir, err := os.MkdirTemp("", "rsg-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	root, err := config.ResolveRoot(dir, "")
	must(t, err)
	r := &runningService{root: root, logs: &bytes.Buffer{}, done: make(chan error, 1)}
	r.logger = newServiceLogger(r.logs)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- runService(ctx, root, false, io.Discard, r.logger) }()
	t.Cleanup(func() {
		cancel()
		if !r.stopped {
			<-r.done
		}
	})
	c := service.NewClient(filepath.Join(dir, "osmia.sock"))
	t.Cleanup(c.Close)
	awaitHealth(t, c, true, 5*time.Second, func() string { return "" })
	return r
}

func (r *runningService) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.stopped = true
		if err != nil {
			t.Fatalf("runService: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop")
	}
}

func (r *runningService) runSecond(t *testing.T) error {
	t.Helper()
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runService(context.Background(), r.root, false, io.Discard, newServiceLogger(&bytes.Buffer{}))
	}()
	select {
	case err := <-secondDone:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("second runService on the same root did not return promptly")
		return nil
	}
}

func TestRunServiceRoutesTheStandardLogPackageThroughItsStructuredLogger(t *testing.T) {
	r := startRunService(t)
	log.Printf("embedded dependency output %d", 7)
	r.stop(t)

	var sawEmbedded bool
	for _, line := range strings.Split(strings.TrimRight(r.logs.String(), "\n"), "\n") {
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
		t.Fatalf("no structured record for the standard log package call:\n%s", r.logs.String())
	}
}

func TestRunServiceRefusesASecondServiceOnTheSameRoot(t *testing.T) {
	r := startRunService(t)
	if err := r.runSecond(t); err == nil || !strings.Contains(err.Error(), "already owned by another Osmia service") {
		t.Fatalf("second runService on the same root: %v", err)
	}
}

func TestRunServiceKeepsItsLoggerWhenASecondServiceIsRefused(t *testing.T) {
	r := startRunService(t)
	r.runSecond(t)
	if slog.Default() != r.logger {
		t.Fatal("slog default changed while the first service still runs")
	}
}

func TestRunServiceRestoresTheLoggingGlobalsWhenItStops(t *testing.T) {
	previousDefault := slog.Default()
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	r := startRunService(t)
	r.stop(t)
	if slog.Default() != previousDefault {
		t.Fatal("slog default was not restored after the service stopped")
	}
	if log.Writer() != previousOutput || log.Flags() != previousFlags {
		t.Fatal("standard log package defaults were not restored after the service stopped")
	}
}

type detachedGitHubRun struct {
	project config.ProjectID
	log     []byte
}

func runDetachedIssueHandIn(t *testing.T) detachedGitHubRun {
	t.Helper()
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
	successful(t, root, "handin", string(added.Project.ID), "https://github.com/owner/repo/issues/3")

	code = Run(context.Background(), []string{"stop", "--root", root}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("stop %d", code)
	}
	awaitHealth(t, c, false, 10*time.Second, nil)

	data, err := os.ReadFile(filepath.Join(root, "service.log"))
	must(t, err)
	return detachedGitHubRun{project: added.Project.ID, log: data}
}

func serviceLogRecords(t *testing.T, data []byte) []struct{ Time, Level, Msg, Activity, Access, Action, Project string } {
	t.Helper()
	var records []struct{ Time, Level, Msg, Activity, Access, Action, Project string }
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct{ Time, Level, Msg, Activity, Access, Action, Project string }
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a JSON object: %s: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestDetachedServiceLogsEveryLineAsAStructuredRecord(t *testing.T) {
	run := runDetachedIssueHandIn(t)
	for _, record := range serviceLogRecords(t, run.log) {
		if record.Time == "" || record.Level == "" || record.Msg == "" {
			t.Fatalf("record is missing a timestamp, level or message: %+v", record)
		}
	}
}

func TestDetachedServiceLogsGitHubActionsOnRealStandardError(t *testing.T) {
	run := runDetachedIssueHandIn(t)
	var sawGitHub bool
	for _, record := range serviceLogRecords(t, run.log) {
		if record.Activity == "github" {
			sawGitHub = true
			if record.Access != "read" || record.Action != "issues.fetch" || record.Project != string(run.project) {
				t.Fatalf("github record: %+v", record)
			}
		}
	}
	if !sawGitHub {
		t.Fatalf("service.log has no GitHub activity record:\n%s", run.log)
	}
}

func TestDetachedReadinessTimeoutCoversEngineCreation(t *testing.T) {
	if detachedReadinessTimeout <= engine.ActionTimeout+time.Second {
		t.Fatal("readiness cannot cover managed engine creation")
	}
}

func detachWithChild(t *testing.T, script string, timeout time.Duration) (string, error) {
	t.Helper()
	root, err := config.ResolveRoot(t.TempDir(), "")
	must(t, err)
	original := detachedCommand
	t.Cleanup(func() { detachedCommand = original })
	var child *exec.Cmd
	detachedCommand = func(string) (*exec.Cmd, error) {
		child = exec.Command("sh", "-c", script)
		return child, nil
	}
	t.Cleanup(func() {
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
		}
	})
	var out bytes.Buffer
	err = detachWithin(t.Context(), root, &out, timeout)
	return out.String(), err
}

func TestDetachReportsReadyOnceTheChildSignalsReadiness(t *testing.T) {
	out, err := detachWithChild(t, "sleep 0.2; printf 'ready\\n' >&3; exec sleep 10", 2*time.Second)
	must(t, err)
	if !strings.Contains(out, "Osmia is ready") {
		t.Fatal(out)
	}
}

func TestDetachFailsWhenTheChildNeverSignalsReadiness(t *testing.T) {
	_, err := detachWithChild(t, "exec sleep 10", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "readiness timed out") {
		t.Fatalf("%v", err)
	}
}
