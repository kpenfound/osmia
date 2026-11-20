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

// reportAttempts is how many times one write is tried, each time after
// reconciling it, before the reporter gives up on it.
const reportAttempts = 5

// reportRetryDelay is the delay before the first retry of a failed write.
const reportRetryDelay = 30 * time.Second

// reportPoll is the longest the worker waits before it looks at its state
// again.
const reportPoll = 15 * time.Second

// reportCheckInterval is the least time between reads of one saved issue's
// state.
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
	Now func() time.Time
}

// The kinds of reporter fault, so a success clears only the faults it can
// have fixed.
const (
	faultState  = "state"
	faultToken  = "token"
	faultGitHub = "github"
	faultScrub  = "scrub"
)

// errorReporter files the captured errors of the service as GitHub issues,
// off the request path. Only its worker changes st and the retry state; mu
// lets the status and configuration views read the faults meanwhile.
type errorReporter struct {
	s       *Service
	capture *errorCapture
	done    chan struct{}
	rearm   atomic.Bool
	// gen counts the applied configurations that changed [error_reports]; a
	// pass files only while the generation it started with is current.
	gen atomic.Uint64

	mu      sync.Mutex
	faults  map[string]string
	applied config.ErrorReports

	st     *errorreport.State
	stPath string
	dirty  bool
	retry  reportRetry
}

// reportRetry is the retry state of the unresolved write being reconciled.
type reportRetry struct {
	key      string
	attempts int
	// next is the real time of the next attempt, or when a given-up write may
	// be tried again.
	next   time.Time
	gaveUp bool
}

func newErrorReporter(s *Service, capture *errorCapture) *errorReporter {
	return &errorReporter{s: s, capture: capture, done: make(chan struct{}), faults: map[string]string{}}
}

// errSuperseded marks a write or a read the reporter dropped because the
// configuration changed under it. It is neither a success nor a failure.
var errSuperseded = errors.New("the error reports configuration changed")

// configure applies the loaded [error_reports] section: it starts or stops
// capturing at once, lets a given-up write be tried again, and, when the
// section changed, supersedes the pass that is running so it sends no more
// from the old configuration.
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

// superseded reports whether a configuration change has been applied since
// the environment env was built.
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

// run passes over the captured errors whenever one arrives or the
// configuration changes, when a retry or a state check is due, and every
// poll, until ctx ends.
func (r *errorReporter) run(ctx context.Context) {
	defer close(r.done)
	timer := time.NewTimer(r.poll())
	defer timer.Stop()
	for {
		wait := r.pass(ctx)
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

// reportEnv is what one pass files with.
type reportEnv struct {
	repo    string
	cfg     errorreport.Config
	filer   issues.Filer
	scrub   *errorreport.Scrubber
	sources config.ErrorReports
	// gen is the configuration generation the environment was built from.
	gen uint64
}

// pass files what is due: it reconciles an unresolved write, resolves the
// recurrences awaiting a state check and decides every queued error. It
// returns how long to wait before the next pass is worth making, or zero.
func (r *errorReporter) pass(ctx context.Context) time.Duration {
	if r.rearm.Swap(false) {
		r.retry = reportRetry{}
	}
	// The generation is read before the configuration: a reload stores its
	// configuration before it bumps the generation, so an environment built
	// from the old configuration never carries the new generation.
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
		// A reload changed the configuration: start again with it.
		return time.Millisecond
	}
	return wait
}

// settleOne decides the oldest queued error, or else the next recurrence
// awaiting a check, reading issue states and reconciling an unresolved write
// first as the decision asks. It reports whether something was decided; when
// not, wait says when to try again.
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

// until is how long after now the time at is, at least a millisecond, or zero
// when at is zero.
func (r *errorReporter) until(at, now time.Time) time.Duration {
	if at.IsZero() {
		return 0
	}
	return max(at.Sub(now), time.Millisecond)
}

// renderCheck renders the intent's content, so a report that cannot be
// rendered is never recorded as a write.
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

// reconcile tries the unresolved write: it looks for what the write may have
// produced and adopts it, and only otherwise writes again. A failed attempt is
// retried after a growing delay and given up after reportAttempts; a given-up
// write is tried again when an error is waiting and the delay has passed, or
// after a reload. It reports whether the write is resolved; when not, wait says
// when to try again.
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

// try performs the write of an intent that is saved, directly when it was just
// recorded or after reconciling it otherwise, and settles the retry state with
// the outcome.
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
		// A write sent before the reload failed: the intent counts the
		// attempt and stays for reconciliation, but the failure is not the
		// current configuration's to report.
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

// settle reconciles the intent: it adopts the issue or comment the write may
// already have produced, and writes only when none exists.
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

// findIssue looks in the intent's repository for the issue the intent's create
// may have produced: one carrying the model's marker for the fingerprint,
// created since the intent was recorded. It may try the issues carrying the
// intent's labels first, but concludes there is none only after listing every
// issue updated since, whatever its labels.
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

// findComment looks through the comments of the intent's issue since the intent
// was recorded for the one the intent's comment may have produced.
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

// write renders the intent and posts it, and records the result.
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

// readState reads whether the saved issue is open or closed. An issue GitHub no
// longer has counts as closed, so the next report opens a new one.
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

// save writes the state if it changed. A failure is a fault and nothing is
// written to GitHub until the state can be saved.
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

// githubReason describes a failed GitHub call. It never holds the token.
func githubReason(err error) string {
	reason := redact.Secrets(err.Error(), redact.KnownSecrets()...)
	var failed *issues.Error
	if errors.As(err, &failed) && (failed.Status == http.StatusUnauthorized || failed.Status == http.StatusForbidden || failed.Status == http.StatusNotFound) {
		reason += " (check that GITHUB_TOKEN can create and read issues and comments on the repository)"
	}
	return reason
}

// diagnostics reports why error reports are not being filed.
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
