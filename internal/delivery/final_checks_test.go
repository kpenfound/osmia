package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	reviewedCommit = "1111111111111111111111111111111111111111"
	pushedCommit   = "2222222222222222222222222222222222222222"
	cloudToken     = "cloud-token-for-tests"
)

// watchClock is the fake clock of a watch fixture.
type watchClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *watchClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *watchClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeStatusAPI stands in for GitHub's combined status read of the pushed
// commit and for the Dagger Cloud JSON the "Run Dagger Checks" status points at.
type fakeStatusAPI struct {
	mu       sync.Mutex
	clock    *watchClock
	statuses []pulls.CommitStatus
	docs     map[string]string
	reads    []time.Time
	commits  []string
	err      error
}

func (f *fakeStatusAPI) CommitStatuses(_ context.Context, repository, commit string) (pulls.CommitStatuses, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, f.clock.Now())
	f.commits = append(f.commits, repository+"@"+commit)
	if f.err != nil {
		return pulls.CommitStatuses{}, f.err
	}
	return pulls.CommitStatuses{Repository: repository, Commit: commit, Statuses: append([]pulls.CommitStatus(nil), f.statuses...)}, nil
}

func (f *fakeStatusAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.docs[r.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func (f *fakeStatusAPI) set(statuses ...pulls.CommitStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = statuses
}

func (f *fakeStatusAPI) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func (f *fakeStatusAPI) doc(id int64, body string) string {
	target := fmt.Sprintf("https://dagger.cloud/checks/%d.json", id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[target] = body
	return target
}

func counts(total, passed, failed, cancelled int) string {
	return fmt.Sprintf(`{"dagger":{"status":"completed","checks":{"total":%d,"passed":%d,"failed":%d,"cancelled":%d}}}`, total, passed, failed, cancelled)
}

type watchFixture struct {
	t        *testing.T
	cfg      *config.Config
	repo     *trace.Repository
	clock    *watchClock
	api      *fakeStatusAPI
	reviewer *FinalReviewer
	report   FinalReport
	pushedAt time.Time
	rerunLog string
	program  string
	scope    *coreadapter.Scope
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	cfg, repo := deliveryTrace(t)
	cfg.Project.PullRequestPoll = "10m"
	cfg.Project.ChecksTimeout = "2h"
	f := &watchFixture{t: t, cfg: cfg, repo: repo, clock: &watchClock{at: fixturekit.DemoStart.Add(time.Hour)}}
	f.api = &fakeStatusAPI{clock: f.clock, docs: map[string]string{}}
	f.pushedAt = f.clock.Now()
	f.rerunLog = filepath.Join(t.TempDir(), "rerun.log")
	f.program = filepath.Join(t.TempDir(), "dagger")
	script := fmt.Sprintf("#!/bin/sh\n{ printf 'args=%%s\\n' \"$*\"; printf 'pwd=%%s\\n' \"$(pwd -P)\"; printf 'token=%%s\\n' \"$DAGGER_CLOUD_TOKEN\"; printf 'ci=%%s\\n' \"${CI-unset}\"; } >> '%s'\n", f.rerunLog)
	fixturekit.Must(t, os.WriteFile(f.program, []byte(script), 0700))
	f.reviewer = f.newReviewer()
	publisher := &Publisher{S: f.reviewer.S, Repository: repo}
	fixturekit.Must(t, publisher.RecordPublication(context.Background(), stream, DeliveryPublication{Push: 1, Review: 1, Operation: "review", Status: PublicationOpened, Reviewed: reviewedCommit, Commit: pushedCommit, PullRequest: 7, URL: "https://github.com/acme/widgets/pull/7",
		Upstream: "acme/widgets", Fork: "me/widgets", Branch: featureBranch(stream), Base: "main", Draft: true, PushedAt: f.pushedAt}))
	f.report = FinalReport{Review: 1, Operation: "review", Commit: reviewedCommit}
	return f
}

// newReviewer is a reviewer over fresh service state, as a restarted service
// has: nothing is remembered but the trace.
func (f *watchFixture) newReviewer() *FinalReviewer {
	controller := New(Dependencies{
		Now:   f.clock.Now,
		Step:  func(string) error { return nil },
		About: func(*trace.Repository) *config.Config { return f.cfg },
		ConversationTrace: func(string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *api.APIError) {
			return f.repo.Project(), stream, f.repo, nil
		},
		CloudStatuses: f.api, CloudStatusFetcher: checkrun.CloudStatusFetcher{HTTP: &http.Client{Transport: f.api}, Allow: func(u *url.URL) bool { return u.Hostname() == "dagger.cloud" }},
		CloudRerun: checkrun.DaggerChecks{Program: f.program, CloudToken: cloudToken},
	})
	return &FinalReviewer{S: controller, Repository: f.repo}
}

// restart forgets everything but the trace.
func (f *watchFixture) restart() {
	polled.Delete(pollKey{f.repo, stream, 1})
	f.reviewer = f.newReviewer()
}

func (f *watchFixture) check() (FinalCheckRun, error) {
	f.t.Helper()
	return f.reviewer.FinalChecks(context.Background(), f.cfg, stream, f.report)
}

func (f *watchFixture) mustWait() *WaitingChecksError {
	f.t.Helper()
	run, err := f.check()
	var waiting *WaitingChecksError
	if !errors.As(err, &waiting) {
		f.t.Fatalf("checks %+v: %v", run, err)
	}
	return waiting
}

func (f *watchFixture) mustComplete() FinalCheckRun {
	f.t.Helper()
	run, err := f.check()
	if err != nil {
		f.t.Fatalf("checks %+v: %v", run, err)
	}
	return run
}

// afterWait moves the clock to where the first status read is due.
func (f *watchFixture) afterWait() { f.clock.At(f.pushedAt.Add(FinalChecksWait)) }

func (c *watchClock) At(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

func (f *watchFixture) runs() []FinalCheckRun {
	f.t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, stream)
	fixturekit.Must(f.t, err)
	var out []FinalCheckRun
	for _, d := range docs {
		if d.ID == FinalChecksDocument(1) {
			var run FinalCheckRun
			fixturekit.Must(f.t, json.Unmarshal([]byte(d.Content), &run))
			out = append(out, run)
		}
	}
	return out
}

func (f *watchFixture) rerunCalls() []string {
	data, err := os.ReadFile(f.rerunLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	fixturekit.Must(f.t, err)
	return strings.Split(strings.TrimSpace(string(data)), "args=")[1:]
}

func (f *watchFixture) notices() int {
	f.t.Helper()
	entries, err := f.repo.Outbox(stream)
	fixturekit.Must(f.t, err)
	n := 0
	for _, e := range entries {
		if e.Event.ID == trace.EventID("final-checks-1-push-1-load-missing", "chief") {
			n++
		}
	}
	return n
}

func (f *watchFixture) at(d time.Duration) time.Time { return f.pushedAt.Add(d) }

// loaded is the status of a loaded module.
func (f *watchFixture) loaded() pulls.CommitStatus {
	return pulls.CommitStatus{ID: 1, Context: "load", State: "success", CreatedAt: f.at(30 * time.Second)}
}

func (f *watchFixture) pending(id int64, at time.Duration) pulls.CommitStatus {
	return pulls.CommitStatus{ID: id, Context: "Run Dagger Checks", State: "pending", CreatedAt: f.at(at)}
}

// done is a finished "Run Dagger Checks" posting whose JSON holds counts.
func (f *watchFixture) done(id int64, state string, at time.Duration, body string) pulls.CommitStatus {
	return pulls.CommitStatus{ID: id, Context: "Run Dagger Checks", State: state, CreatedAt: f.at(at), TargetURL: f.api.doc(id, body)}
}

func failedCheck(id int64, name, traceID string, at time.Duration, f *watchFixture) pulls.CommitStatus {
	return pulls.CommitStatus{ID: id, Context: name, State: "failure", Description: "Failed in 2m33s. Run `dagger trace " + traceID + "`", CreatedAt: f.at(at)}
}

func TestFinalChecksMakeNoRequestBeforeTheWaitAndThenFollowThePollInterval(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	f.clock.At(f.at(FinalChecksWait - time.Second))
	waiting := f.mustWait()
	if f.api.readCount() != 0 || !waiting.Next.Equal(f.at(FinalChecksWait)) {
		t.Fatalf("reads %d, next %s", f.api.readCount(), waiting.Next)
	}
	f.afterWait()
	f.mustWait()
	if f.api.readCount() != 1 || f.api.commits[0] != "acme/widgets@"+pushedCommit {
		t.Fatalf("reads %v", f.api.commits)
	}
	// Within the interval nothing is read; at its end the next read is made.
	f.clock.Advance(10*time.Minute - time.Second)
	f.mustWait()
	if f.api.readCount() != 1 {
		t.Fatalf("read inside the poll interval: %d", f.api.readCount())
	}
	f.clock.Advance(time.Second)
	f.mustWait()
	if f.api.readCount() != 2 || !f.api.reads[1].Equal(f.api.reads[0].Add(10*time.Minute)) {
		t.Fatalf("reads %v", f.api.reads)
	}
}

func TestFinalChecksRecordThePushedCommitOnceStatusesCompleteLate(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("a pending run was recorded: %+v", f.runs())
	}
	f.clock.Advance(10 * time.Minute)
	f.api.set(f.loaded(), f.done(3, "success", 5*time.Minute, counts(92, 92, 0, 0)))
	run := f.mustComplete()
	if run.Status != "passed" || run.Source != "pull_request" || run.PullRequest != 7 || run.Push != 1 || run.PushedCommit != pushedCommit || run.Commit != reviewedCommit ||
		run.Counts == nil || run.Counts.Total != 92 || run.Run == nil || run.Run.ID != 3 || run.StatusURL != "https://dagger.cloud/checks/3.json" {
		t.Fatalf("run %+v", run)
	}
	if recorded := f.runs(); len(recorded) != 1 || recorded[0].Status != "passed" {
		t.Fatalf("recorded %+v", recorded)
	}
	// The recorded result is read back without another status read.
	reads := f.api.readCount()
	if again := f.mustComplete(); again.Run.ID != 3 || f.api.readCount() != reads {
		t.Fatalf("again %+v, reads %d", again, f.api.readCount())
	}
	evidence := FinalCheckEvidence(run)
	for _, want := range []string{"pull request #7", "push 1", pushedCommit, "92 checks, 92 passed, 0 failed, 0 cancelled", "https://dagger.cloud/checks/3.json"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("evidence lacks %q:\n%s", want, evidence)
		}
	}
}

func TestFinalChecksSendOneNoticeWhenLoadNeverRanAndKeepWatching(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.api.set()
	for range 3 {
		f.mustWait()
		f.clock.Advance(10 * time.Minute)
	}
	if f.api.readCount() != 3 || f.notices() != 1 || len(f.runs()) != 0 {
		t.Fatalf("reads %d, notices %d, runs %+v", f.api.readCount(), f.notices(), f.runs())
	}
	f.restart()
	f.mustWait()
	if f.notices() != 1 {
		t.Fatalf("a restart repeated the notice: %d", f.notices())
	}
	f.clock.Advance(10 * time.Minute)
	f.api.set(f.loaded(), f.done(3, "success", 20*time.Minute, counts(2, 2, 0, 0)))
	if run := f.mustComplete(); run.Status != "passed" || f.notices() != 1 {
		t.Fatalf("run %+v, notices %d", run, f.notices())
	}
}

func TestFinalChecksRecordAFailedLoadAsAFailedResult(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.api.set(pulls.CommitStatus{ID: 1, Context: "load", State: "failure", Description: "Failed. Run `dagger trace abc123`", CreatedAt: f.at(30 * time.Second)})
	run := f.mustComplete()
	if run.Status != "failed" || len(run.Failed) != 1 || run.Failed[0] != "load" || run.FailedChecks[0].TraceID != "abc123" || len(run.Reruns) != 0 || len(f.rerunCalls()) != 0 {
		t.Fatalf("run %+v, reruns %v", run, f.rerunCalls())
	}
}

func TestFinalChecksRecordIncompleteRunsWithDoublingWaitsWhenPendingPastTheTimeout(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.cfg.Project.ChecksTimeout = "30m"
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	f.afterWait()
	f.mustWait()
	f.clock.At(f.at(30 * time.Minute))
	for n := 1; n <= 3; n++ {
		_, err := f.check()
		if err == nil || !strings.Contains(err.Error(), "did not complete") || !strings.Contains(err.Error(), "checks_timeout 30m0s") {
			t.Fatalf("run %d: %v", n, err)
		}
		count, latest, err := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit)
		fixturekit.Must(t, err)
		if count != n || latest.Status != ChecksIncomplete {
			t.Fatalf("run %d: count %d, latest %+v", n, count, latest)
		}
		// Before the doubling wait is over nothing is read or recorded.
		reads := f.api.readCount()
		f.clock.Advance(FinalCheckDelay(n) - time.Second)
		if _, err := f.check(); err == nil || f.api.readCount() != reads {
			t.Fatalf("run %d read inside its wait: %v", n, err)
		}
		f.clock.Advance(time.Second)
	}
	if FinalCheckDelay(1) != 10*time.Minute || FinalCheckDelay(2) != 20*time.Minute || FinalCheckDelay(3) != 40*time.Minute {
		t.Fatal("waits do not double")
	}
	if n, _, _ := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit); n != 3 {
		t.Fatalf("incomplete runs: %d", n)
	}
	f.api.set(f.loaded(), f.done(3, "success", 40*time.Minute, counts(5, 5, 0, 0)))
	if run := f.mustComplete(); run.Status != "passed" {
		t.Fatalf("run %+v", run)
	}
	if n, latest, _ := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit); n != 0 || latest.Status != "passed" {
		t.Fatalf("incomplete runs after a completed one: %d %+v", n, latest)
	}
}

func TestFinalChecksNeverRecordACancelledCheckAsPassed(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.api.set(f.loaded(), f.done(3, "success", 5*time.Minute, counts(10, 9, 0, 1)))
	run, err := f.check()
	if err == nil || run.Status != ChecksIncomplete || !strings.Contains(run.Error, "1 of 10 checks were cancelled") || run.Counts == nil || run.Counts.Cancelled != 1 {
		t.Fatalf("run %+v: %v", run, err)
	}
	if recorded := f.runs(); len(recorded) != 1 || recorded[0].Status == "passed" {
		t.Fatalf("recorded %+v", recorded)
	}
	// Unaccounted checks are not a pass either.
	g := newWatchFixture(t)
	g.afterWait()
	g.api.set(g.loaded(), g.done(3, "success", 5*time.Minute, counts(10, 8, 0, 0)))
	if run, err := g.check(); err == nil || run.Status != ChecksIncomplete || !strings.Contains(run.Error, "have no result") {
		t.Fatalf("run %+v: %v", run, err)
	}
}

// failingRun makes the pushed commit show a completed failure of two checks.
func (f *watchFixture) failingRun() (original pulls.CommitStatus, checks []pulls.CommitStatus) {
	original = f.done(10, "failure", 100*time.Second, counts(92, 90, 2, 0))
	checks = []pulls.CommitStatus{failedCheck(11, "test:a", "trace-a", 95*time.Second, f), failedCheck(12, "test:b", "trace-b", 95*time.Second, f)}
	f.api.set(append([]pulls.CommitStatus{f.loaded(), original}, checks...)...)
	return original, checks
}

func TestFinalChecksRerunNamesTheOrganizationOnlyWithoutAFork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fork, org, want string
	}{
		{"no fork", "", "acme", "cloud checks rerun --failed --pr 7 --org acme\n"},
		{"fork", "me/widgets", "acme", "cloud checks rerun --failed --pr 7\n"},
		{"no organization", "", "", "cloud checks rerun --failed --pr 7\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchFixture(t)
			f.cfg.Project.Fork, f.cfg.Dagger.Organization = tc.fork, tc.org
			f.afterWait()
			f.failingRun()
			f.mustWait()
			if calls := f.rerunCalls(); len(calls) != 1 || !strings.HasPrefix(calls[0], tc.want) {
				t.Fatalf("rerun calls %q, want %q", calls, tc.want)
			}
			request, err := ReadFinalCheckRerun(f.repo, stream, 1, 1)
			fixturekit.Must(t, err)
			if got := strings.Join(request.Command, " ") + "\n"; got != "dagger "+tc.want {
				t.Fatalf("recorded command %q", got)
			}
		})
	}
}

func TestFinalChecksRerunFailedChecksOnceAndSettleOnlyOnAFreshCompletion(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	original, checks := f.failingRun()

	f.mustWait()
	calls := f.rerunCalls()
	clone, _ := filepath.EvalSymlinks(f.cfg.Project.Clone)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "cloud checks rerun --failed --pr 7\n") || !strings.Contains(calls[0], "pwd="+clone+"\n") || !strings.Contains(calls[0], "token="+cloudToken+"\n") || !strings.Contains(calls[0], "ci=unset") {
		t.Fatalf("rerun calls %q", calls)
	}
	request, err := ReadFinalCheckRerun(f.repo, stream, 1, 1)
	fixturekit.Must(t, err)
	if request == nil || request.Push != 1 || request.PullRequest != 7 || !request.RequestedAt.Equal(f.clock.Now()) || request.Original.ID != 10 || len(request.Failed) != 2 || request.Failed[0].Name != "test:a" || request.Failed[0].TraceID != "trace-a" || request.Failed[0].StatusID != 11 || request.FinishedAt.IsZero() {
		t.Fatalf("rerun record %+v", request)
	}

	// The original failure keeps showing for several polls: it settles nothing.
	for range 3 {
		f.clock.Advance(10 * time.Minute)
		waiting := f.mustWait()
		if !strings.Contains(waiting.Reason, "posted before the rerun was requested") {
			t.Fatalf("waiting %v", waiting)
		}
	}
	if len(f.runs()) != 0 || len(f.rerunCalls()) != 1 {
		t.Fatalf("runs %+v, reruns %d", f.runs(), len(f.rerunCalls()))
	}

	// A different posting made before the request is not the rerun's either.
	f.api.set(append([]pulls.CommitStatus{f.loaded(), f.done(9, "failure", 80*time.Second, counts(92, 90, 2, 0))}, checks...)...)
	f.clock.Advance(10 * time.Minute)
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("runs %+v", f.runs())
	}

	// A pending posting is not a result either.
	f.clock.Advance(10 * time.Minute)
	f.api.set(append([]pulls.CommitStatus{f.loaded(), f.pending(13, 60*time.Minute)}, checks...)...)
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("runs %+v", f.runs())
	}

	// The fresh completion passes, and the originally failed checks are flakes.
	f.clock.Advance(10 * time.Minute)
	fresh := f.done(14, "success", f.clock.Now().Sub(f.pushedAt), counts(92, 92, 0, 0))
	f.api.set(f.loaded(), fresh, pulls.CommitStatus{ID: 15, Context: "test:a", State: "success", CreatedAt: f.clock.Now()})
	run := f.mustComplete()
	if run.Status != "passed" || len(run.Reruns) != 1 || run.Reruns[0].Original.ID != original.ID || run.Reruns[0].Fresh == nil || run.Reruns[0].Fresh.ID != 14 || len(run.Reruns[0].Flakes) != 2 || len(run.Flaky) != 2 || len(run.Failed) != 0 {
		t.Fatalf("run %+v", run)
	}
	evidence := FinalCheckEvidence(run)
	if !strings.Contains(evidence, "Flaky, failed and then passed on rerun: test:a, test:b") {
		t.Fatalf("evidence:\n%s", evidence)
	}
	if len(f.rerunCalls()) != 1 {
		t.Fatalf("rerun calls: %d", len(f.rerunCalls()))
	}
}

func TestFinalChecksKeepACheckFailedThatFailsItsFreshRerun(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.failingRun()
	f.mustWait()
	f.clock.Advance(10 * time.Minute)
	now := f.clock.Now().Sub(f.pushedAt)
	// test:a passes its rerun, test:b fails again with a new trace.
	f.api.set(f.loaded(), f.done(20, "failure", now, counts(92, 91, 1, 0)),
		pulls.CommitStatus{ID: 21, Context: "test:a", State: "success", CreatedAt: f.clock.Now()},
		failedCheck(22, "test:b", "trace-b2", now, f))
	run := f.mustComplete()
	if run.Status != "failed" || len(run.Failed) != 1 || run.Failed[0] != "test:b" || run.FailedChecks[0].TraceID != "trace-b2" || len(run.Reruns) != 1 || len(run.Reruns[0].Flakes) != 1 || run.Reruns[0].Flakes[0] != "test:a" || len(run.Flaky) != 1 {
		t.Fatalf("run %+v", run)
	}
	if !strings.Contains(FinalCheckEvidence(run), "- test:b: `dagger trace trace-b2`") {
		t.Fatalf("evidence:\n%s", FinalCheckEvidence(run))
	}
}

func TestFinalChecksRecordIncompleteAndNeverTheOriginalFailureWhenNoFreshResultArrives(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.cfg.Project.ChecksTimeout = "30m"
	f.afterWait()
	f.failingRun()
	f.mustWait()
	requested := f.clock.Now()
	f.clock.At(requested.Add(20 * time.Minute))
	f.mustWait()
	f.clock.At(requested.Add(30 * time.Minute))
	run, err := f.check()
	if err == nil || run.Status != ChecksIncomplete || !strings.Contains(run.Error, "within checks_timeout 30m0s of the rerun request") || len(run.Reruns) != 1 || run.Reruns[0].Fresh != nil {
		t.Fatalf("run %+v: %v", run, err)
	}
	for _, recorded := range f.runs() {
		if recorded.Status == "failed" || len(recorded.Failed) > 0 {
			t.Fatalf("the original failure was recorded: %+v", recorded)
		}
	}
	// Watching goes on after the wait; still no fallback to the old failure.
	f.clock.Advance(FinalCheckDelay(1))
	if run, err := f.check(); err == nil || run.Status != ChecksIncomplete || len(f.rerunCalls()) != 1 {
		t.Fatalf("run %+v: %v, reruns %d", run, err, len(f.rerunCalls()))
	}
}

func TestFinalChecksResumeAfterARestartWithoutRepeatingARerunOrANotice(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	// During the wait.
	f.clock.At(f.at(time.Minute))
	f.restart()
	if waiting := f.mustWait(); !waiting.Next.Equal(f.at(FinalChecksWait)) || f.api.readCount() != 0 {
		t.Fatalf("waiting %v, reads %d", waiting, f.api.readCount())
	}
	// While pending.
	f.afterWait()
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	f.mustWait()
	f.restart()
	f.mustWait()
	// After the rerun was requested, with the original failure still showing.
	f.clock.Advance(10 * time.Minute)
	original, checks := f.failingRun()
	f.mustWait()
	if len(f.rerunCalls()) != 1 {
		t.Fatalf("rerun calls %d", len(f.rerunCalls()))
	}
	f.restart()
	f.clock.Advance(10 * time.Minute)
	if waiting := f.mustWait(); !strings.Contains(waiting.Reason, "posted before the rerun was requested") || len(f.rerunCalls()) != 1 || len(f.runs()) != 0 {
		t.Fatalf("waiting %v, reruns %d, runs %+v", waiting, len(f.rerunCalls()), f.runs())
	}
	// After the fresh posting starts arriving.
	f.clock.Advance(10 * time.Minute)
	f.api.set(append([]pulls.CommitStatus{f.loaded(), f.pending(30, f.clock.Now().Sub(f.pushedAt))}, checks...)...)
	f.mustWait()
	f.restart()
	f.clock.Advance(10 * time.Minute)
	f.api.set(f.loaded(), f.done(31, "success", f.clock.Now().Sub(f.pushedAt), counts(92, 92, 0, 0)))
	run := f.mustComplete()
	if run.Status != "passed" || run.Reruns[0].Original.ID != original.ID || len(f.rerunCalls()) != 1 || len(run.Flaky) != 2 {
		t.Fatalf("run %+v, reruns %d", run, len(f.rerunCalls()))
	}
	f.restart()
	if again := f.mustComplete(); again.Run.ID != 31 || f.notices() != 0 {
		t.Fatalf("again %+v", again)
	}
}

func TestFinalChecksNeverRerunWhenTheRerunRecordExistsWithoutItsResult(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	original, _ := f.failingRun()
	// A service that stopped between recording the request and running it.
	w := &finalWatch{a: f.reviewer, cfg: f.cfg, stream: stream, report: f.report, push: Push{Number: 1, PullRequest: 7, Commit: pushedCommit}}
	fixturekit.Must(t, w.writeRerun(context.Background(), FinalCheckRerunRecord{Review: 1, Push: 1, PullRequest: 7, PushedCommit: pushedCommit, Command: []string{"dagger"}, CheckRerun: rerunOf(original, f.clock.Now())}))
	f.restart()
	f.mustWait()
	if len(f.rerunCalls()) != 0 {
		t.Fatalf("rerun repeated: %q", f.rerunCalls())
	}
}

func TestFinalChecksReadOlderLocalRecords(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	old := `{"review":1,"attempt":2,"operation":"review","commit":"` + reviewedCommit + `","status":"passed","command":["dagger","check"],"exit_code":0,"output":"ok","failed":["dag://?check=x"],"flaky":["dag://?check=y"],"started_at":"2026-01-02T03:04:05Z","finished_at":"2026-01-02T03:05:05Z"}`
	content := []byte(old + "\n")
	fixturekit.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalChecksDocument(1), Revision: 1, Project: f.repo.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: checksActor, Cause: "review"}, Path: FinalChecksPath(1), Content: string(content)}}))
	run := f.mustComplete()
	if run.Source != "" || run.Status != "passed" || run.Attempt != 2 || f.api.readCount() != 0 {
		t.Fatalf("run %+v", run)
	}
	if evidence := FinalCheckEvidence(run); !strings.Contains(evidence, "The run passed") || !strings.Contains(evidence, "dag://?check=y") {
		t.Fatalf("evidence:\n%s", evidence)
	}
	var decoded FinalCheckRun
	fixturekit.Must(t, json.NewDecoder(bytes.NewReader(content)).Decode(&decoded))
}

func rerunOf(original pulls.CommitStatus, at time.Time) (r api.CheckRerun) {
	r.RequestedAt = at
	r.Original.ID = original.ID
	return r
}
