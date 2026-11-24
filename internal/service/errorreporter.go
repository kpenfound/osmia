package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/errorreport"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/redact"
)

const reportAttempts = 5

const reportRetryDelay = 30 * time.Second

const reportPoll = 15 * time.Second

const reportCheckInterval = time.Minute

// ErrorReporterOptions tunes the error reporter. Zero values select the
// defaults. Tests point BaseURL at a fake GitHub.
type ErrorReporterOptions struct {
	// BaseURL is the GitHub REST API base URL. It defaults to
	// https://api.github.com.
	BaseURL string
	// HTTP is the client for the reporter's GitHub calls. It defaults to one
	// with a 30 second timeout.
	HTTP *http.Client
	// Retry is the delay before a failed write is tried again, doubled after
	// each further failure. It defaults to 30 seconds.
	Retry time.Duration
	// Poll is the longest the worker waits before it looks at its state
	// again. It defaults to 15 seconds.
	Poll time.Duration
	// CheckInterval is the least time between reads of one saved issue's
	// state; a recurrence inside it waits as awaiting a check. It defaults to
	// one minute.
	CheckInterval time.Duration
	// Queue is how many captured errors wait for the worker. It defaults to
	// 256.
	Queue int
	// Now is the reporter's clock, for the days and windows that caps and
	// comments count. It defaults to the service's clock.
	Now    func() time.Time
	passed func()
}

const (
	faultState  = "state"
	faultToken  = "token"
	faultGitHub = "github"
	faultScrub  = "scrub"
)

type errorReporter struct {
	s       *Service
	capture *errorCapture
	done    chan struct{}
	rearm   atomic.Bool
	gen     atomic.Uint64

	mu      sync.Mutex
	faults  map[string]string
	applied config.ErrorReports

	st     *errorreport.State
	stPath string
	dirty  bool
	retry  reportRetry
}

type reportRetry struct {
	key      string
	attempts int
	next     time.Time
	gaveUp   bool
}

func newErrorReporter(s *Service, capture *errorCapture) *errorReporter {
	return &errorReporter{s: s, capture: capture, done: make(chan struct{}), faults: map[string]string{}}
}

var errSuperseded = errors.New("the error reports configuration changed")

func (r *errorReporter) configure(cfg *config.Config) {
	if r == nil {
		return
	}
	r.mu.Lock()
	changed := !reflect.DeepEqual(r.applied, cfg.ErrorReports)
	r.applied = cfg.ErrorReports
	if changed {
		r.gen.Add(1)
	}
	r.mu.Unlock()
	r.capture.configure(cfg.ErrorReports)
	r.rearm.Store(true)
}

func (r *errorReporter) superseded(env reportEnv) bool { return r.gen.Load() != env.gen }

func (r *errorReporter) opts() ErrorReporterOptions { return r.s.options.ErrorReporter }

func (r *errorReporter) now() time.Time {
	if r.opts().Now != nil {
		return r.opts().Now().UTC()
	}
	return r.s.now()
}

func (r *errorReporter) retryDelay() time.Duration {
	if d := r.opts().Retry; d > 0 {
		return d
	}
	return reportRetryDelay
}

func (r *errorReporter) poll() time.Duration {
	if d := r.opts().Poll; d > 0 {
		return d
	}
	return reportPoll
}

func (r *errorReporter) checkInterval() time.Duration {
	if d := r.opts().CheckInterval; d > 0 {
		return d
	}
	return reportCheckInterval
}

func (r *errorReporter) token() string {
	getenv := r.s.options.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv("GITHUB_TOKEN")
}

func (r *errorReporter) run(ctx context.Context) {
	defer close(r.done)
	timer := time.NewTimer(r.poll())
	defer timer.Stop()
	for {
		wait := r.pass(ctx)
		if passed := r.opts().passed; passed != nil {
			passed()
		}
		if wait <= 0 || wait > r.poll() {
			wait = r.poll()
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.capture.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

type reportEnv struct {
	repo    string
	cfg     errorreport.Config
	filer   issues.Filer
	scrub   *errorreport.Scrubber
	sources config.ErrorReports
	gen     uint64
}

func (r *errorReporter) pass(ctx context.Context) time.Duration {
	if r.rearm.Swap(false) {
		r.retry = reportRetry{}
	}
	gen := r.gen.Load()
	cfg := r.s.current()
	e := cfg.ErrorReports
	if !e.Enabled {
		r.clearFaults()
		return 0
	}
	path, err := cfg.Root.ErrorReports()
	if err != nil {
		r.fail(faultState, "cannot resolve the error report state under the root; nothing is filed")
		return 0
	}
	if r.st == nil || r.stPath != path {
		st, err := errorreport.LoadState(path)
		if err != nil {
			r.fail(faultState, "cannot read "+path+"; nothing is filed until it can be read")
			return 0
		}
		r.st, r.stPath, r.dirty = st, path, false
	}
	token := r.token()
	if token == "" {
		r.fail(faultToken, "GITHUB_TOKEN is not set; error reports cannot be filed on "+e.Repository+" until it is")
		return 0
	}
	r.clear(faultToken)
	build := r.s.options.Build
	env := reportEnv{repo: e.Repository, sources: e, gen: gen}
	env.cfg = errorreport.Config{Repository: e.Repository, Labels: slices.Clone(e.Labels), CheckInterval: r.checkInterval(), Version: build.Version, Commit: build.Commit}
	env.cfg.MaxIssuesPerHour, env.cfg.MaxIssuesPerDay, env.cfg.MaxCommentsPerDay = e.MaxIssuesPerHour, e.MaxIssuesPerDay, e.MaxCommentsPerDay
	env.filer = issues.Filer{BaseURL: r.opts().BaseURL, Token: token, HTTP: r.opts().HTTP}
	env.scrub = errorreport.NewScrubber(errorreport.ScrubOptions{Repository: e.Repository, BuildCommit: build.Commit, Secrets: []string{token}})
	if env.filer.HTTP == nil {
		env.filer.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	var wait time.Duration
	for ctx.Err() == nil && !r.superseded(env) {
		if n := r.capture.takeDropped(); n > 0 {
			r.st.NoteQueueDrops(n)
			r.dirty = true
		}
		var progress bool
		progress, wait = r.settleOne(ctx, env)
		if !progress {
			break
		}
	}
	r.save()
	if r.superseded(env) {
		return time.Millisecond
	}
	return wait
}

func (r *errorReporter) settleOne(ctx context.Context, env reportEnv) (progress bool, wait time.Duration) {
	if r.superseded(env) {
		return false, 0
	}
	head, haveHead := r.capture.head()
	var rep *errorreport.Report
	if haveHead {
		if !env.sources.SourceEnabled(string(head.event.Source)) {
			r.capture.consume(head)
			return true, 0
		}
		report, err := env.scrub.Report(head.event)
		if err != nil {
			r.capture.consume(head)
			r.failScrub(err)
			return true, 0
		}
		rep = &report
	}
	var check *errorreport.Check
	for ctx.Err() == nil {
		now := r.now()
		d := errorreport.Decide(r.st, env.cfg, errorreport.Input{Report: rep, Check: check}, now)
		check = nil
		if r.superseded(env) {
			return false, 0
		}
		switch d.Kind {
		case errorreport.Reconcile:
			if ok, wait := r.reconcile(ctx, env, *d.Intent, haveHead); !ok {
				return false, wait
			}
		case errorreport.CheckState:
			state, err := r.readState(ctx, env, d.Issue)
			if err != nil {
				if ctx.Err() != nil || r.superseded(env) {
					return false, 0
				}
				r.failGitHub("reading the state of the issue saved for an error report", err)
				r.st.NoteCheckAttempt(env.repo, d.Fingerprint, now)
				r.dirty = true
				continue
			}
			if r.superseded(env) {
				return false, 0
			}
			r.clear(faultGitHub)
			check = &errorreport.Check{Repository: env.repo, Issue: d.Issue, State: state, At: now}
		case errorreport.Idle:
			return false, r.until(d.NotBefore, now)
		case errorreport.Count, errorreport.OverCap, errorreport.AwaitCheck:
			if haveHead && !r.capture.consume(head) {
				return true, 0
			}
			r.st.Apply(d)
			r.dirty = true
			return true, 0
		case errorreport.OpenIssue, errorreport.OpenLinkedIssue, errorreport.Comment:
			if haveHead && !r.capture.consume(head) {
				return true, 0
			}
			if err := renderCheck(*d.Intent); err != nil {
				r.failScrub(err)
				return true, 0
			}
			r.st.Apply(d)
			r.dirty = true
			if !r.save() {
				return false, 0
			}
			r.try(ctx, env, *d.Intent, true)
			return true, 0
		default:
			return false, 0
		}
	}
	return false, 0
}

func (r *errorReporter) until(at, now time.Time) time.Duration {
	if at.IsZero() {
		return 0
	}
	return max(at.Sub(now), time.Millisecond)
}

func renderCheck(in errorreport.Intent) error {
	if in.Kind == errorreport.IntentComment {
		_, err := errorreport.RenderComment(in.Filing)
		return err
	}
	_, err := errorreport.RenderIssue(in.Filing)
	return err
}

func intentKey(in errorreport.Intent) string {
	return fmt.Sprintf("%s/%s/%s/%s", in.Kind, errorreport.RepoKey(in.Repository), in.Fingerprint, in.Day)
}

func (r *errorReporter) reconcile(ctx context.Context, env reportEnv, in errorreport.Intent, haveHead bool) (bool, time.Duration) {
	key := intentKey(in)
	if r.retry.key != key {
		r.retry = reportRetry{key: key}
	}
	if wait := time.Until(r.retry.next); wait > 0 {
		return false, wait
	}
	if r.retry.gaveUp {
		if !haveHead {
			return false, 0
		}
		r.retry = reportRetry{key: key}
	}
	if !r.save() {
		return false, 0
	}
	return r.try(ctx, env, in, false)
}

func (r *errorReporter) try(ctx context.Context, env reportEnv, in errorreport.Intent, fresh bool) (bool, time.Duration) {
	var err error
	if fresh {
		err = r.write(ctx, env, in)
	} else {
		err = r.settle(ctx, env, in)
	}
	if err == nil {
		r.retry = reportRetry{}
		r.clear(faultGitHub, faultScrub)
		return true, 0
	}
	if ctx.Err() != nil || errors.Is(err, errSuperseded) {
		return false, 0
	}
	var scrubbed *errorreport.ScrubError
	if errors.As(err, &scrubbed) {
		r.st.AbandonIntent(in)
		r.dirty = true
		r.retry = reportRetry{}
		r.failScrub(err)
		return true, 0
	}
	if r.superseded(env) {
		r.st.NoteAttempt(in)
		r.dirty = true
		r.save()
		return false, 0
	}
	key := intentKey(in)
	if r.retry.key != key {
		r.retry = reportRetry{key: key}
	}
	r.retry.attempts++
	r.st.NoteAttempt(in)
	r.dirty = true
	delay := r.retryDelay() << (r.retry.attempts - 1)
	r.retry.next = time.Now().Add(delay)
	gaveUp := r.retry.attempts >= reportAttempts
	r.retry.gaveUp = gaveUp
	what := "filing an error report"
	if in.Kind == errorreport.IntentComment {
		what = "commenting on an error report issue"
	}
	r.failGitHubAttempt(what, err, r.retry.attempts, gaveUp)
	r.s.log().Warn("error report write failed", "component", "error_reporter", "kind", string(in.Kind), "attempts", r.retry.attempts, "gave_up", gaveUp, "error", redact.Secrets(err.Error(), redact.KnownSecrets()...))
	r.save()
	return false, delay
}

func (r *errorReporter) settle(ctx context.Context, env reportEnv, in errorreport.Intent) error {
	switch in.Kind {
	case errorreport.IntentCreate:
		issue, found, err := r.findIssue(ctx, env, in)
		if r.superseded(env) {
			return errSuperseded
		}
		if err != nil {
			return err
		}
		if found {
			return r.completeCreate(in, issue.Number, true)
		}
	case errorreport.IntentComment:
		found, err := r.findComment(ctx, env, in)
		if r.superseded(env) {
			return errSuperseded
		}
		if err != nil {
			return err
		}
		if found {
			return r.completeComment(in, true)
		}
	}
	return r.write(ctx, env, in)
}

func (r *errorReporter) findIssue(ctx context.Context, env reportEnv, in errorreport.Intent) (issues.Issue, bool, error) {
	pick := func(list []issues.Issue) (issues.Issue, bool) {
		var found issues.Issue
		ok := false
		for _, issue := range list {
			if !errorreport.MatchesIssue(issue.Body, in.Fingerprint) || !in.CreatedByIntent(issue.CreatedAt) {
				continue
			}
			if !ok || issue.CreatedAt.Before(found.CreatedAt) {
				found, ok = issue, true
			}
		}
		return found, ok
	}
	if len(in.Labels) > 0 {
		if list, err := env.filer.ListIssues(ctx, in.Repository, in.Since(), in.Labels); err == nil {
			if issue, ok := pick(list); ok {
				return issue, true, nil
			}
		}
	}
	if r.superseded(env) {
		return issues.Issue{}, false, errSuperseded
	}
	list, err := env.filer.ListIssues(ctx, in.Repository, in.Since(), nil)
	if err != nil {
		return issues.Issue{}, false, err
	}
	issue, ok := pick(list)
	return issue, ok, nil
}

func (r *errorReporter) findComment(ctx context.Context, env reportEnv, in errorreport.Intent) (bool, error) {
	list, err := env.filer.ListComments(ctx, in.Repository, in.Issue, in.Since())
	if err != nil {
		return false, err
	}
	for _, c := range list {
		if errorreport.MatchesComment(c.Body, in.Fingerprint, in.Day) && in.CreatedByIntent(c.CreatedAt) {
			return true, nil
		}
	}
	return false, nil
}

func (r *errorReporter) write(ctx context.Context, env reportEnv, in errorreport.Intent) error {
	if r.superseded(env) {
		return errSuperseded
	}
	switch in.Kind {
	case errorreport.IntentCreate:
		rendered, err := errorreport.RenderIssue(in.Filing)
		if err != nil {
			return err
		}
		issue, err := env.filer.Create(ctx, in.Repository, issues.NewIssue{Title: rendered.Title, Body: rendered.Body, Labels: in.Labels})
		if err != nil {
			return err
		}
		return r.completeCreate(in, issue.Number, false)
	case errorreport.IntentComment:
		body, err := errorreport.RenderComment(in.Filing)
		if err != nil {
			return err
		}
		if _, err := env.filer.Comment(ctx, in.Repository, in.Issue, body); err != nil {
			return err
		}
		return r.completeComment(in, false)
	}
	return fmt.Errorf("unknown write %q", in.Kind)
}

func (r *errorReporter) completeCreate(in errorreport.Intent, issue int, adopted bool) error {
	if err := r.st.CompleteCreate(in, issue, r.now()); err != nil {
		return err
	}
	r.dirty = true
	r.save()
	r.s.log().Info("error report issue recorded", "component", "error_reporter", "repository", in.Repository, "issue", issue, "adopted", adopted, "source", string(in.Filing.Report.Source))
	return nil
}

func (r *errorReporter) completeComment(in errorreport.Intent, adopted bool) error {
	if err := r.st.CompleteComment(in); err != nil {
		return err
	}
	r.dirty = true
	r.save()
	r.s.log().Info("error report comment recorded", "component", "error_reporter", "repository", in.Repository, "issue", in.Issue, "adopted", adopted)
	return nil
}

func (r *errorReporter) readState(ctx context.Context, env reportEnv, issue int) (errorreport.IssueState, error) {
	state, err := env.filer.State(ctx, env.repo, issue)
	if err != nil {
		var failed *issues.Error
		if errors.As(err, &failed) && (failed.Status == http.StatusNotFound || failed.Status == http.StatusGone) {
			return errorreport.IssueClosed, nil
		}
		return "", err
	}
	if state == issues.StateClosed {
		return errorreport.IssueClosed, nil
	}
	return errorreport.IssueOpen, nil
}

func (r *errorReporter) save() bool {
	if !r.dirty {
		return true
	}
	if err := r.st.Save(r.stPath); err != nil {
		r.fail(faultState, "cannot record error reports in "+r.stPath+"; nothing is filed until it can be written")
		return false
	}
	r.dirty = false
	r.clear(faultState)
	return true
}

func (r *errorReporter) fail(kind, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults[kind] = message
}

func (r *errorReporter) clear(kinds ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range kinds {
		delete(r.faults, k)
	}
}

func (r *errorReporter) clearFaults() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.faults)
}

func (r *errorReporter) failScrub(err error) {
	reason := "the report could not be built"
	var scrubbed *errorreport.ScrubError
	if errors.As(err, &scrubbed) {
		reason = scrubbed.Reason
	}
	r.fail(faultScrub, "an error report was not filed because it could not be made safe to publish: "+reason)
	r.s.log().Warn("error report not filed", "component", "error_reporter", "reason", reason)
}

func (r *errorReporter) failGitHub(what string, err error) {
	r.fail(faultGitHub, what+" failed: "+githubReason(err)+"; it is tried again later")
}

func (r *errorReporter) failGitHubAttempt(what string, err error, attempt int, gaveUp bool) {
	if gaveUp {
		r.fail(faultGitHub, fmt.Sprintf("gave up %s after %d attempts: %s", what, attempt, githubReason(err)))
		return
	}
	r.fail(faultGitHub, fmt.Sprintf("%s failed (attempt %d of %d): %s; retrying", what, attempt, reportAttempts, githubReason(err)))
}

func githubReason(err error) string {
	reason := redact.Secrets(err.Error(), redact.KnownSecrets()...)
	var failed *issues.Error
	if errors.As(err, &failed) && (failed.Status == http.StatusUnauthorized || failed.Status == http.StatusForbidden || failed.Status == http.StatusNotFound) {
		reason += " (check that GITHUB_TOKEN can create and read issues and comments on the repository)"
	}
	return reason
}

func (r *errorReporter) diagnostics() []Diagnostic {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Diagnostic
	for _, kind := range []string{faultToken, faultState, faultGitHub, faultScrub} {
		if message := r.faults[kind]; message != "" {
			out = append(out, Diagnostic{Field: "error_reports", Code: Unavailable, Message: message})
		}
	}
	return out
}
