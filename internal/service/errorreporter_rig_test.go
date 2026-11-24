package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/errorreport"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const (
	reportRepo  = "owner/osmia"
	reportToken = "ghp_0123456789abcdefghijklmnopqrstuvwxyzABCD"
)

type reporterRig struct {
	t       *testing.T
	opts    Options
	files   reloadFiles
	gh      *fixturekit.FakeIssueServer
	clock   *fixturekit.IssueClock
	log     *fixturekit.SyncBuffer
	tokenOn atomic.Bool
	passes  atomic.Int64
	s       *Service
	c       *Client
	stopped bool
}

func section(repository string, extra ...string) string {
	return "[error_reports]\nenabled = true\nrepository = \"" + repository + "\"\n" + strings.Join(extra, "\n") + "\n"
}

func newReporterRig(t *testing.T, text string, tune func(*ErrorReporterOptions)) *reporterRig {
	t.Helper()
	r := &reporterRig{t: t, log: &fixturekit.SyncBuffer{}}
	r.clock = fixturekit.NewIssueClock(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	r.gh = fixturekit.NewFakeIssueServer(t)
	r.gh.Now = r.clock.Now
	r.tokenOn.Store(true)
	r.opts = fixture(t)
	r.opts.Logger = slog.New(slog.NewJSONHandler(r.log, nil))
	r.opts.ErrorReporter = ErrorReporterOptions{BaseURL: r.gh.URL(), HTTP: r.gh.Client(), Retry: 5 * time.Millisecond, Poll: 5 * time.Millisecond, CheckInterval: time.Minute, Now: r.clock.Now}
	r.opts.ErrorReporter.passed = func() { r.passes.Add(1) }
	if tune != nil {
		tune(&r.opts.ErrorReporter)
	}
	r.opts.getenv = func(name string) string {
		if name == "GITHUB_TOKEN" {
			if r.tokenOn.Load() {
				return reportToken
			}
			return ""
		}
		return os.Getenv(name)
	}
	r.files = configFiles(t, r.opts)
	if text != "" {
		r.files.write(t, r.files.topText+text)
	}
	t.Cleanup(r.stop)
	r.launch()
	return r
}

func (r *reporterRig) launch() {
	r.t.Helper()
	s, err := Start(context.Background(), r.opts)
	fixturekit.Must(r.t, err)
	r.s, r.c, r.stopped = s, NewClient(s.Socket()), false
}

func (r *reporterRig) stop() {
	if r.stopped {
		return
	}
	r.stopped = true
	r.c.Close()
	if err := r.s.Close(); err != nil {
		r.t.Logf("service close: %v", err)
	}
}

func (r *reporterRig) restart() {
	r.t.Helper()
	r.stop()
	r.launch()
}

func (r *reporterRig) reload(text string) {
	r.t.Helper()
	r.files.write(r.t, r.files.topText+text)
	_, err := r.c.Reload(context.Background())
	fixturekit.Must(r.t, err)
}

func (r *reporterRig) apiError() {
	r.t.Helper()
	r.s.ready.Store(false)
	_ = r.c.Do(context.Background(), "GET", "/v1/health", nil, nil)
	r.s.ready.Store(true)
}

func (r *reporterRig) opFail(action, message string) {
	r.t.Helper()
	r.s.log().Error("operation failed", "project", "p_0123456789abcdef", "workstream", "w_0123456789abcdef", "action", action, "boundary", "workspace", "duration", "1s", "error", message)
}

func (r *reporterRig) githubFail(method, path string, status int, message string) {
	r.t.Helper()
	ctx := ghlog.WithLogger(context.Background(), r.s.log())
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "pulls.find", Access: ghlog.Read, Repository: "acme/private-repo", Method: method, Path: path, Status: status, Err: errors.New(message)})
}

func (r *reporterRig) gitFail() {
	r.t.Helper()
	ctx := ghlog.WithLogger(context.Background(), r.s.log())
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "workspace.push", Access: ghlog.Write, Repository: "acme/private-repo", Remote: "origin", Branch: "osmia/w-1", Err: errors.New("push rejected")})
}

func (r *reporterRig) drain() {
	r.t.Helper()
	intakefixture.Soon(r.t, "the error queue to empty", func() bool { return r.s.reporter.capture.queued() == 0 })
	r.awaitPasses(2)
}

func (r *reporterRig) awaitIssues(repository string, n int) []fixturekit.FakeIssue {
	r.t.Helper()
	intakefixture.Soon(r.t, "issues to be filed", func() bool { return len(r.gh.Issues(repository)) >= n })
	return r.gh.Issues(repository)
}

func (r *reporterRig) awaitComments(repository string, issue, n int) []fixturekit.FakeComment {
	r.t.Helper()
	intakefixture.Soon(r.t, "comments to be posted", func() bool { return len(r.gh.Comments(repository, issue)) >= n })
	return r.gh.Comments(repository, issue)
}

// awaitPasses waits for n more passes of the reporter's worker to finish. A
// pass may be running already, so two passes include one that began after the
// call.
func (r *reporterRig) awaitPasses(n int64) {
	r.t.Helper()
	intakefixture.AwaitFurtherPasses(r.t, &r.passes, n)
}

func (r *reporterRig) quiet(repository string, n int) {
	r.t.Helper()
	r.drain()
	if got := len(r.gh.Issues(repository)); got != n {
		r.t.Fatalf("%s holds %d issues, want %d: %+v", repository, got, n, r.gh.Issues(repository))
	}
}

func (r *reporterRig) state() *errorreport.State {
	r.t.Helper()
	st, err := errorreport.LoadState(r.statePath())
	fixturekit.Must(r.t, err)
	return st
}

func (r *reporterRig) statePath() string {
	return filepath.Join(r.opts.Config.Root, "error-reports.json")
}

func (r *reporterRig) awaitResolved() {
	r.t.Helper()
	intakefixture.Soon(r.t, "the unresolved write to be resolved", func() bool {
		st, err := errorreport.LoadState(r.statePath())
		return err == nil && len(st.Intents) == 0 && len(st.Repositories) > 0
	})
}

func (r *reporterRig) diagnostics() (configuration, status []string) {
	r.t.Helper()
	cfg, err := r.c.Configuration(context.Background())
	fixturekit.Must(r.t, err)
	st, err := r.c.Statuses(context.Background())
	fixturekit.Must(r.t, err)
	for _, d := range cfg.Diagnostics {
		if d.Field == "error_reports" {
			configuration = append(configuration, d.Message)
		}
	}
	for _, d := range st.Diagnostics {
		if d.Field == "error_reports" {
			status = append(status, d.Message)
		}
	}
	return configuration, status
}

func (r *reporterRig) awaitDiagnostic(want string) {
	r.t.Helper()
	intakefixture.Soon(r.t, "the reporter diagnostic "+want, func() bool {
		cfg, st := r.diagnostics()
		return containsAny(cfg, want) && containsAny(st, want)
	})
}

func (r *reporterRig) awaitNoDiagnostic() {
	r.t.Helper()
	intakefixture.Soon(r.t, "the reporter diagnostic to clear", func() bool {
		cfg, st := r.diagnostics()
		return len(cfg) == 0 && len(st) == 0
	})
}

func containsAny(messages []string, want string) bool {
	for _, m := range messages {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

func sourceOf(body string) string {
	const label = "- **Source:** `"
	i := strings.Index(body, label)
	if i < 0 {
		return ""
	}
	rest := body[i+len(label):]
	if j := strings.Index(rest, "`"); j >= 0 {
		return rest[:j]
	}
	return ""
}
