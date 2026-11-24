package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

type fakeStatusAPI struct {
	mu       sync.Mutex
	clock    *watchClock
	statuses []pulls.CommitStatus
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

func (f *fakeStatusAPI) setWithChecks(n int, statuses ...pulls.CommitStatus) {
	f.set(append(statuses, passing(n)...)...)
}

func passing(n int) []pulls.CommitStatus {
	var out []pulls.CommitStatus
	for i := range n {
		out = append(out, pulls.CommitStatus{ID: int64(1000 + i), Context: fmt.Sprintf("check-%03d", i), State: "success", Description: "Passed in 1m."})
	}
	return out
}

type watchFixture struct {
	t            *testing.T
	cfg          *config.Config
	repo         *trace.Repository
	clock        *watchClock
	api          *fakeStatusAPI
	reviewer     *FinalReviewer
	report       FinalReport
	pushedAt     time.Time
	rerunLog     string
	program      string
	scope        *coreadapter.Scope
	reader       pulls.StatusReader
	traces       checkrun.TraceRunner
	traceProgram string
	cliTimeout   time.Duration
	autoTimeout  time.Duration
	step         func(string) error
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	cfg, repo := deliveryTrace(t)
	cfg.Project.PullRequestPoll = "10m"
	cfg.Project.ChecksTimeout = "2h"
	f := &watchFixture{t: t, cfg: cfg, repo: repo, clock: &watchClock{at: fixturekit.DemoStart.Add(time.Hour)}}
	f.api = &fakeStatusAPI{clock: f.clock}
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

func (f *watchFixture) newReviewer() *FinalReviewer {
	var statuses pulls.StatusReader = f.api
	if f.reader != nil {
		statuses = f.reader
	}
	var traces checkrun.TraceRunner
	switch {
	case f.traces != nil:
		traces = f.traces
	case f.traceProgram != "":
		traces = checkrun.DaggerChecks{Program: f.traceProgram, CloudToken: cloudToken, TraceTimeout: f.cliTimeout}
	}
	controller := New(Dependencies{
		Now:   f.clock.Now,
		Step:  func(string) error { return nil },
		About: func(*trace.Repository) *config.Config { return f.cfg },
		ConversationTrace: func(string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *api.APIError) {
			return f.repo.Project(), stream, f.repo, nil
		},
		CloudStatuses: statuses,
		CloudRerun:    checkrun.DaggerChecks{Program: f.program, CloudToken: cloudToken},
	})
	controller.CloudTrace, controller.AutoTraceTimeout = traces, f.autoTimeout
	controller.Step = func(name string) error {
		if f.step != nil {
			return f.step(name)
		}
		return nil
	}
	return &FinalReviewer{S: controller, Repository: f.repo}
}

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

func (f *watchFixture) loaded() pulls.CommitStatus {
	return pulls.CommitStatus{ID: 1, Context: "load", State: "success", CreatedAt: f.at(30 * time.Second)}
}

func (f *watchFixture) pending(id int64, at time.Duration) pulls.CommitStatus {
	return pulls.CommitStatus{ID: id, Context: "Run Dagger Checks", State: "pending", CreatedAt: f.at(at)}
}

func (f *watchFixture) done(id int64, state string, at time.Duration) pulls.CommitStatus {
	return pulls.CommitStatus{ID: id, Context: "Run Dagger Checks", State: state, CreatedAt: f.at(at), TargetURL: fmt.Sprintf("https://dagger.cloud/checks/%d", id)}
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
	f.api.setWithChecks(92, f.loaded(), f.done(3, "success", 5*time.Minute))
	run := f.mustComplete()
	if run.Status != "passed" || run.Source != "pull_request" || run.PullRequest != 7 || run.Push != 1 || run.PushedCommit != pushedCommit || run.Commit != reviewedCommit ||
		run.Counts == nil || run.Counts.Total != 92 || run.Run == nil || run.Run.ID != 3 || run.StatusURL != "https://dagger.cloud/checks/3" {
		t.Fatalf("run %+v", run)
	}
	if recorded := f.runs(); len(recorded) != 1 || recorded[0].Status != "passed" {
		t.Fatalf("recorded %+v", recorded)
	}
	reads := f.api.readCount()
	if again := f.mustComplete(); again.Run.ID != 3 || f.api.readCount() != reads {
		t.Fatalf("again %+v, reads %d", again, f.api.readCount())
	}
	evidence := FinalCheckEvidence(run)
	for _, want := range []string{"pull request #7", "push 1", pushedCommit, "92 checks, 92 passed, 0 failed, 0 cancelled", "https://dagger.cloud/checks/3"} {
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
	f.api.setWithChecks(2, f.loaded(), f.done(3, "success", 20*time.Minute))
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
	f.api.setWithChecks(5, f.loaded(), f.done(3, "success", 40*time.Minute))
	if run := f.mustComplete(); run.Status != "passed" {
		t.Fatalf("run %+v", run)
	}
	if n, latest, _ := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit); n != 0 || latest.Status != "passed" {
		t.Fatalf("incomplete runs after a completed one: %d %+v", n, latest)
	}
}

func (f *watchFixture) settleIncomplete(statuses ...pulls.CommitStatus) (FinalCheckRun, error) {
	f.t.Helper()
	f.api.set(statuses...)
	run, err := f.check()
	var waiting *WaitingChecksError
	if err == nil || errors.As(err, &waiting) {
		f.t.Fatalf("run %+v: %v", run, err)
	}
	return run, err
}

func TestFinalChecksNeverRecordAnUnaccountedCompletedRunAsPassed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		checks   []pulls.CommitStatus
		wantText string
	}{
		{"zero checks", nil, "the run reports no checks"},
		{"a pending check", append(passing(9), pulls.CommitStatus{ID: 50, Context: "test:slow", State: "pending"}), "1 of 10 checks have no result"},
		{"only pending checks", []pulls.CommitStatus{{ID: 50, Context: "a", State: "pending"}, {ID: 51, Context: "b", State: "pending"}}, "2 of 2 checks have no result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchFixture(t)
			f.afterWait()
			run, err := f.settleIncomplete(append([]pulls.CommitStatus{f.loaded(), f.done(3, "success", 5*time.Minute)}, tc.checks...)...)
			if run.Status != ChecksIncomplete || !strings.Contains(run.Error, tc.wantText) || run.Counts == nil || run.Counts.Cancelled != 0 || run.StatusURL != "https://dagger.cloud/checks/3" || !strings.Contains(err.Error(), "the completed run is not a pass") {
				t.Fatalf("run %+v: %v", run, err)
			}
			if recorded := f.runs(); len(recorded) != 1 || recorded[0].Status != ChecksIncomplete {
				t.Fatalf("recorded %+v", recorded)
			}
			for range 2 {
				f.clock.Advance(FinalCheckDelay(1) * 4)
				if run, _ := f.settleIncomplete(append([]pulls.CommitStatus{f.loaded(), f.done(3, "success", 5*time.Minute)}, tc.checks...)...); run.Status != ChecksIncomplete {
					t.Fatalf("run %+v", run)
				}
			}
			for _, recorded := range f.runs() {
				if recorded.Status != ChecksIncomplete {
					t.Fatalf("recorded %+v", recorded)
				}
			}
		})
	}
}

func (f *watchFixture) stuckOnIncomplete() {
	f.t.Helper()
	f.cfg.Project.ChecksTimeout = "30m"
	f.afterWait()
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	f.mustWait()
	f.clock.At(f.at(30 * time.Minute))
	for n := 1; n <= 2; n++ {
		if _, err := f.check(); err == nil || !strings.Contains(err.Error(), "did not complete") {
			f.t.Fatalf("run %d: %v", n, err)
		}
		f.clock.Advance(FinalCheckDelay(n))
	}
	if n, _, _ := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit); n != 2 {
		f.t.Fatalf("incomplete runs: %d", n)
	}
}

func TestFinalChecksRecordAPassFromStatusesAloneAfterIncompleteRecords(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.stuckOnIncomplete()
	f.api.setWithChecks(3, f.loaded(), f.done(3, "success", 40*time.Minute))
	run := f.mustComplete()
	if run.Status != "passed" || run.ExitCode != 0 || run.Counts == nil || *run.Counts != (api.CheckCounts{Total: 3, Passed: 3}) || run.StatusURL != "https://dagger.cloud/checks/3" || run.Run == nil || run.Run.ID != 3 {
		t.Fatalf("run %+v", run)
	}
	recorded := f.runs()
	if last := recorded[len(recorded)-1]; last.Status != "passed" || len(recorded) != 3 {
		t.Fatalf("recorded %+v", recorded)
	}
	if n, latest, _ := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit); n != 0 || latest.Status != "passed" {
		t.Fatalf("incomplete runs after the pass: %d %+v", n, latest)
	}
	if len(f.rerunCalls()) != 0 {
		t.Fatalf("rerun calls %q", f.rerunCalls())
	}
}

func TestFinalChecksRecordAFailureFromStatusesAloneAfterIncompleteRecords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		statuses func(f *watchFixture) []pulls.CommitStatus
		failed   []string
	}{
		{"the run failed", func(f *watchFixture) []pulls.CommitStatus {
			return append([]pulls.CommitStatus{f.loaded(), f.done(10, "failure", 41*time.Minute)}, passing(3)...)
		}, nil},
		{"the run errored", func(f *watchFixture) []pulls.CommitStatus {
			return append([]pulls.CommitStatus{f.loaded(), f.done(10, "error", 41*time.Minute)}, passing(3)...)
		}, nil},
		{"a check failed", func(f *watchFixture) []pulls.CommitStatus {
			return append([]pulls.CommitStatus{f.loaded(), f.done(10, "success", 41*time.Minute), failedCheck(11, "test:a", "trace-a", 40*time.Minute, f)}, passing(3)...)
		}, []string{"test:a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchFixture(t)
			f.stuckOnIncomplete()
			f.api.set(tc.statuses(f)...)
			f.mustWait()
			if calls := f.rerunCalls(); len(calls) != 1 || !strings.HasPrefix(calls[0], "cloud checks rerun --failed --pr 7") {
				t.Fatalf("rerun calls %q", calls)
			}
			f.clock.Advance(10 * time.Minute)
			fresh := f.clock.Now().Sub(f.pushedAt)
			statuses := tc.statuses(f)
			statuses[1] = f.done(20, statuses[1].State, fresh)
			for i := range statuses {
				if statuses[i].Context != "load" && statuses[i].Context != "Run Dagger Checks" && statuses[i].State == "failure" {
					statuses[i].ID, statuses[i].CreatedAt = 21, f.clock.Now()
				}
			}
			f.api.set(statuses...)
			run := f.mustComplete()
			if run.Status != "failed" || run.ExitCode != 1 || !slices.Equal(run.Failed, tc.failed) || len(run.Reruns) != 1 || run.Run == nil || run.Run.ID != 20 {
				t.Fatalf("run %+v", run)
			}
			if tc.failed != nil && run.FailedChecks[0].TraceID != "trace-a" {
				t.Fatalf("failed checks %+v", run.FailedChecks)
			}
			if len(f.rerunCalls()) != 1 {
				t.Fatalf("rerun calls %q", f.rerunCalls())
			}
		})
	}
}

func TestFinalChecksStayIncompleteWhenOnlyZeroOrPendingChecksReportAfterIncompleteRecords(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.stuckOnIncomplete()
	run, _ := f.settleIncomplete(f.loaded(), f.done(3, "success", 40*time.Minute))
	if run.Status != ChecksIncomplete || !strings.Contains(run.Error, "the run reports no checks") {
		t.Fatalf("run %+v", run)
	}
	f.clock.Advance(FinalCheckDelay(3))
	run, _ = f.settleIncomplete(append([]pulls.CommitStatus{f.loaded(), f.done(3, "success", 40*time.Minute), {ID: 60, Context: "test:slow", State: "pending"}}, passing(2)...)...)
	if run.Status != ChecksIncomplete || !strings.Contains(run.Error, "1 of 3 checks have no result") {
		t.Fatalf("run %+v", run)
	}
	for _, recorded := range f.runs() {
		if recorded.Status == "passed" {
			t.Fatalf("recorded %+v", recorded)
		}
	}
}

func (f *watchFixture) failingRun() (original pulls.CommitStatus, checks []pulls.CommitStatus) {
	original = f.done(10, "failure", 100*time.Second)
	checks = []pulls.CommitStatus{failedCheck(11, "test:a", "trace-a", 95*time.Second, f), failedCheck(12, "test:b", "trace-b", 95*time.Second, f)}
	f.api.setWithChecks(90, append([]pulls.CommitStatus{f.loaded(), original}, checks...)...)
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

	f.api.set(append([]pulls.CommitStatus{f.loaded(), f.done(9, "failure", 80*time.Second)}, checks...)...)
	f.clock.Advance(10 * time.Minute)
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("runs %+v", f.runs())
	}

	f.clock.Advance(10 * time.Minute)
	f.api.set(append([]pulls.CommitStatus{f.loaded(), f.pending(13, 60*time.Minute)}, checks...)...)
	f.mustWait()
	if len(f.runs()) != 0 {
		t.Fatalf("runs %+v", f.runs())
	}

	f.clock.Advance(10 * time.Minute)
	fresh := f.done(14, "success", f.clock.Now().Sub(f.pushedAt))
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
	f.api.set(f.loaded(), f.done(20, "failure", now),
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
	f.clock.Advance(FinalCheckDelay(1))
	if run, err := f.check(); err == nil || run.Status != ChecksIncomplete || len(f.rerunCalls()) != 1 {
		t.Fatalf("run %+v: %v, reruns %d", run, err, len(f.rerunCalls()))
	}
}

func TestFinalChecksResumeAfterARestartWithoutRepeatingARerunOrANotice(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.clock.At(f.at(time.Minute))
	f.restart()
	if waiting := f.mustWait(); !waiting.Next.Equal(f.at(FinalChecksWait)) || f.api.readCount() != 0 {
		t.Fatalf("waiting %v, reads %d", waiting, f.api.readCount())
	}
	f.afterWait()
	f.api.set(f.loaded(), f.pending(2, 40*time.Second))
	f.mustWait()
	f.restart()
	f.mustWait()
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
	f.clock.Advance(10 * time.Minute)
	f.api.set(append([]pulls.CommitStatus{f.loaded(), f.pending(30, f.clock.Now().Sub(f.pushedAt))}, checks...)...)
	f.mustWait()
	f.restart()
	f.clock.Advance(10 * time.Minute)
	f.api.setWithChecks(92, f.loaded(), f.done(31, "success", f.clock.Now().Sub(f.pushedAt)))
	run := f.mustComplete()
	if run.Status != "passed" || run.Reruns[0].Original.ID != original.ID || len(f.rerunCalls()) != 1 || len(run.Flaky) != 2 {
		t.Fatalf("run %+v, reruns %d", run, len(f.rerunCalls()))
	}
	f.restart()
	if again := f.mustComplete(); again.Run.ID != 31 || f.notices() != 0 {
		t.Fatalf("again %+v", again)
	}
}

func TestFinalChecksRecordARefusedRerunAsIncompleteAndRequestItAgainAfterTheWait(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	refuse := filepath.Join(t.TempDir(), "refuse")
	fixturekit.Must(t, os.WriteFile(refuse, nil, 0600))
	script := fmt.Sprintf("#!/bin/sh\nprintf 'args=%%s\\n' \"$*\" >> '%s'\nif [ -e '%s' ]; then echo 'Error: fetch Cloud checks for github.com/acme/widgets: GetOrgChecks: unauthorized; invalid API token type' >&2; exit 1; fi\n", f.rerunLog, refuse)
	fixturekit.Must(t, os.WriteFile(f.program, []byte(script), 0700))
	f.afterWait()
	original, _ := f.failingRun()

	run, err := f.check()
	if err == nil || run.Status != ChecksIncomplete || !strings.Contains(run.Error, "Dagger Cloud refused the rerun") || !strings.Contains(run.Error, "dagger.cloud_api_token_env") || len(run.Reruns) != 1 || !strings.Contains(run.Reruns[0].Error, "invalid API token type") || len(run.Failed) != 0 {
		t.Fatalf("run %+v: %v", run, err)
	}
	request, err := ReadFinalCheckRerun(f.repo, stream, 1, 1)
	fixturekit.Must(t, err)
	if !request.refused() || request.Attempt != 1 || len(f.rerunCalls()) != 1 {
		t.Fatalf("rerun record %+v, calls %d", request, len(f.rerunCalls()))
	}

	f.clock.Advance(FinalCheckDelay(1) - time.Minute)
	if _, err := f.check(); err == nil || len(f.rerunCalls()) != 1 {
		t.Fatalf("checks before the wait: %v, calls %d", err, len(f.rerunCalls()))
	}

	f.clock.Advance(time.Minute)
	f.restart()
	if run, err := f.check(); err == nil || run.Status != ChecksIncomplete || len(f.rerunCalls()) != 2 {
		t.Fatalf("second refusal %+v: %v, calls %d", run, err, len(f.rerunCalls()))
	}
	if n, _, err := f.reviewer.IncompleteChecks(stream, 1, reviewedCommit); err != nil || n != 2 {
		t.Fatalf("incomplete runs %d, %v", n, err)
	}

	fixturekit.Must(t, os.Remove(refuse))
	f.clock.Advance(FinalCheckDelay(2))
	if waiting := f.mustWait(); !strings.Contains(waiting.Reason, "the failed checks were rerun") {
		t.Fatalf("waiting %v", waiting)
	}
	request, err = ReadFinalCheckRerun(f.repo, stream, 1, 1)
	fixturekit.Must(t, err)
	if request.refused() || request.Attempt != 3 || len(f.rerunCalls()) != 3 {
		t.Fatalf("rerun record %+v, calls %d", request, len(f.rerunCalls()))
	}
	f.clock.Advance(10 * time.Minute)
	f.api.setWithChecks(92, f.loaded(), f.done(40, "success", f.clock.Now().Sub(f.pushedAt)))
	run = f.mustComplete()
	if run.Status != "passed" || len(run.Reruns) != 1 || run.Reruns[0].Original.ID != original.ID || len(run.Flaky) != 2 || len(f.rerunCalls()) != 3 {
		t.Fatalf("run %+v, calls %d", run, len(f.rerunCalls()))
	}
}

func TestFinalChecksNeverRerunWhenTheRerunRecordExistsWithoutItsResult(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	original, _ := f.failingRun()
	w := &finalWatch{a: f.reviewer, cfg: f.cfg, stream: stream, report: f.report, push: Push{Number: 1, PullRequest: 7, Commit: pushedCommit}}
	fixturekit.Must(t, w.writeRerun(context.Background(), FinalCheckRerunRecord{Review: 1, Push: 1, PullRequest: 7, PushedCommit: pushedCommit, Command: []string{"dagger"}, CheckRerun: rerunOf(original, f.clock.Now())}))
	f.restart()
	f.mustWait()
	if len(f.rerunCalls()) != 0 {
		t.Fatalf("rerun repeated: %q", f.rerunCalls())
	}
}

func TestAnExistingRecordOfTheReviewedCommitAndPushCompletesTheWatchWithoutAStatusRead(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	recorded := `{"review":1,"operation":"review","commit":"` + reviewedCommit + `","source":"pull_request","pull_request":7,"push":1,"pushed_commit":"` + pushedCommit + `","status":"passed","exit_code":0,"failed":["dag://?check=x"],"flaky":["dag://?check=y"],"started_at":"2026-01-02T03:04:05Z","finished_at":"2026-01-02T03:05:05Z"}`
	content := []byte(recorded + "\n")
	fixturekit.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalChecksDocument(1), Revision: 1, Project: f.repo.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: checksActor, Cause: "review"}, Path: FinalChecksPath(1), Content: string(content)}}))
	run := f.mustComplete()
	if run.Source != api.CheckSourcePullRequest || run.Status != "passed" || f.api.readCount() != 0 {
		t.Fatalf("run %+v", run)
	}
	if evidence := FinalCheckEvidence(run); !strings.Contains(evidence, "The result is passed") {
		t.Fatalf("evidence:\n%s", evidence)
	}
}

func rerunOf(original pulls.CommitStatus, at time.Time) (r api.CheckRerun) {
	r.RequestedAt = at
	r.Original.ID = original.ID
	return r
}

type statusTransport func(*http.Request) (*http.Response, error)

func (f statusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFinalChecksCarryTheStatusReadErrorWithoutAnyBodyText(t *testing.T) {
	t.Parallel()
	const sentinel = "SENTINEL-GITHUB-BODY"
	const token = "ghp_SENTINELTOKEN0123456789"
	for _, tc := range []struct {
		name         string
		code         int
		contentType  string
		body         string
		want, absent []string
	}{
		{"undecodable answer", 200, "text/html", "<html>" + sentinel + " " + token + "</html>", []string{"GitHub answered 200", `content type "text/html"`, "could not be decoded", "invalid JSON"}, []string{"malformed JSON"}},
		{"refusal", 403, "application/json", `{"message":"` + sentinel + `","errors":[{"message":"` + sentinel + `"}],"documentation_url":"https://` + sentinel + `.example"}`, []string{"GitHub answered 403", `content type "application/json"`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchFixture(t)
			f.cfg.Project.ChecksTimeout = "30m"
			f.reader = pulls.GitHub{BaseURL: "https://api.example", Token: token, HTTP: &http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.code, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}}
			f.restart()
			f.afterWait()
			check := func(label, text string) {
				t.Helper()
				for _, want := range tc.want {
					if !strings.Contains(text, want) {
						t.Errorf("%s %q lacks %q", label, text, want)
					}
				}
				for _, banned := range append([]string{"SENTINEL", token, "Authorization", "Bearer", "<html>", "documentation_url"}, tc.absent...) {
					if strings.Contains(text, banned) {
						t.Errorf("%s %q names %q", label, text, banned)
					}
				}
			}
			waiting := f.mustWait()
			check("waiting error", waiting.Error())
			f.clock.At(f.at(30 * time.Minute))
			run, err := f.check()
			if err == nil || run.Status != ChecksIncomplete {
				t.Fatalf("run %+v: %v", run, err)
			}
			check("retry error", err.Error())
			check("incomplete record", run.Error)
			recorded := f.runs()
			if len(recorded) != 1 {
				t.Fatalf("recorded %+v", recorded)
			}
			check("stored record", recorded[0].Error)
		})
	}
}
