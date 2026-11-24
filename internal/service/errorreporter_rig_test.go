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
)

const (
	reportRepo  = "owner/osmia"
	reportToken = "ghp_0123456789abcdefghijklmnopqrstuvwxyzABCD"
)

// reporterRig is a running service whose error reporter files to a fake
// GitHub, with a clock the test moves.
type reporterRig struct {
	t       *testing.T
	opts    Options
	files   reloadFiles
	gh      *fixturekit.FakeIssueServer
	clock   *fixturekit.IssueClock
	log     *syncBuffer
	tokenOn atomic.Bool
	s       *Service
	c       *Client
	stopped bool
}

// section is an enabled [error_reports] section for repository.
func section(repository string, extra ...string) string {
	return "[error_reports]\nenabled = true\nrepository = \"" + repository + "\"\n" + strings.Join(extra, "\n") + "\n"
}

// newReporterRig starts a service whose top-level configuration holds text
// after the fixture's own. tune adjusts the reporter's options.
func newReporterRig(t *testing.T, text string, tune func(*ErrorReporterOptions)) *reporterRig {
	t.Helper()
	r := &reporterRig{t: t, log: &syncBuffer{}}
	r.clock = fixturekit.NewIssueClock(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	r.gh = fixturekit.NewFakeIssueServer(t)
	r.gh.Now = r.clock.Now
	r.tokenOn.Store(true)
	r.opts = fixture(t)
	r.opts.Logger = slog.New(slog.NewJSONHandler(r.log, nil))
	r.opts.ErrorReporter = ErrorReporterOptions{BaseURL: r.gh.URL(), HTTP: r.gh.Client(), Retry: 5 * time.Millisecond, Poll: 5 * time.Millisecond, CheckInterval: time.Minute, Now: r.clock.Now}
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
	must(r.t, err)
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

// reload replaces the error reports section and reloads the service.
func (r *reporterRig) reload(text string) {
	r.t.Helper()
	r.files.write(r.t, r.files.topText+text)
	_, err := r.c.Reload(context.Background())
	must(r.t, err)
}

// apiError makes the service answer 503 to a real request.
func (r *reporterRig) apiError() {
	r.t.Helper()
	r.s.ready.Store(false)
	_ = r.c.Do(context.Background(), "GET", "/v1/health", nil, nil)
	r.s.ready.Store(true)
}

// opFail logs the record the reconciliation controller writes for a failed
// operation.
func (r *reporterRig) opFail(action, message string) {
	r.t.Helper()
	r.s.log().Error("operation failed", "project", "p_0123456789abcdef", "workstream", "w_0123456789abcdef", "action", action, "boundary", "workspace", "duration", "1s", "error", message)
}

// githubFail logs the activity record of a GitHub REST request that failed.
func (r *reporterRig) githubFail(method, path string, status int, message string) {
	r.t.Helper()
	ctx := ghlog.WithLogger(context.Background(), r.s.log())
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "pulls.find", Access: ghlog.Read, Repository: "acme/private-repo", Method: method, Path: path, Status: status, Err: errors.New(message)})
}

// gitFail logs the activity record of a failed git remote operation.
func (r *reporterRig) gitFail() {
	r.t.Helper()
	ctx := ghlog.WithLogger(context.Background(), r.s.log())
	ghlog.Log(ctx, time.Now(), ghlog.Entry{Action: "workspace.push", Access: ghlog.Write, Repository: "acme/private-repo", Remote: "origin", Branch: "osmia/w-1", Err: errors.New("push rejected")})
}

// drain waits until the worker has decided everything queued and GitHub
// traffic has gone quiet.
func (r *reporterRig) drain() {
	r.t.Helper()
	soon(r.t, "the error queue to empty", func() bool { return r.s.reporter.capture.queued() == 0 })
	settled(r.t, func() int { return len(r.gh.Requests()) })
}

// awaitIssues waits until repository holds n issues and returns them.
func (r *reporterRig) awaitIssues(repository string, n int) []fixturekit.FakeIssue {
	r.t.Helper()
	soon(r.t, "issues to be filed", func() bool { return len(r.gh.Issues(repository)) >= n })
	return r.gh.Issues(repository)
}

// awaitComments waits until the issue holds n comments.
func (r *reporterRig) awaitComments(repository string, issue, n int) []fixturekit.FakeComment {
	r.t.Helper()
	soon(r.t, "comments to be posted", func() bool { return len(r.gh.Comments(repository, issue)) >= n })
	return r.gh.Comments(repository, issue)
}

// quiet fails if the number of issues of repository is not n after the
// reporter has had time to act on anything pending.
func (r *reporterRig) quiet(repository string, n int) {
	r.t.Helper()
	r.drain()
	time.Sleep(60 * time.Millisecond)
	if got := len(r.gh.Issues(repository)); got != n {
		r.t.Fatalf("%s holds %d issues, want %d: %+v", repository, got, n, r.gh.Issues(repository))
	}
}

// state reads the reporter's saved state file.
func (r *reporterRig) state() *errorreport.State {
	r.t.Helper()
	st, err := errorreport.LoadState(r.statePath())
	must(r.t, err)
	return st
}

func (r *reporterRig) statePath() string {
	return filepath.Join(r.opts.Config.Root, "error-reports.json")
}

// awaitResolved waits until the saved state holds no unresolved write.
func (r *reporterRig) awaitResolved() {
	r.t.Helper()
	soon(r.t, "the unresolved write to be resolved", func() bool {
		st, err := errorreport.LoadState(r.statePath())
		return err == nil && len(st.Intents) == 0 && len(st.Repositories) > 0
	})
}

// diagnostics returns the error report diagnostics of the configuration view
// and of the status view.
func (r *reporterRig) diagnostics() (configuration, status []string) {
	r.t.Helper()
	cfg, err := r.c.Configuration(context.Background())
	must(r.t, err)
	st, err := r.c.Statuses(context.Background())
	must(r.t, err)
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

// awaitDiagnostic waits until both views show a diagnostic containing want.
func (r *reporterRig) awaitDiagnostic(want string) {
	r.t.Helper()
	soon(r.t, "the reporter diagnostic "+want, func() bool {
		cfg, st := r.diagnostics()
		return containsAny(cfg, want) && containsAny(st, want)
	})
}

// awaitNoDiagnostic waits until neither view shows a reporter diagnostic.
func (r *reporterRig) awaitNoDiagnostic() {
	r.t.Helper()
	soon(r.t, "the reporter diagnostic to clear", func() bool {
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

// sourceOf reads the source an issue body reports.
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
