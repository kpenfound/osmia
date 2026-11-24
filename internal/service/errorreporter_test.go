package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

// With no [error_reports] section the reporter is off: the three kinds of
// error record are captured nowhere and the fake GitHub receives nothing.
func TestErrorReporterIsOffWithoutASection(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, "", nil)
	r.apiError()
	r.opFail("workspace.draft", "draft failed")
	r.githubFail("GET", "/repos/acme/private-repo/pulls", 502, "bad gateway")
	if n := r.s.reporter.capture.queued(); n != 0 {
		t.Fatalf("%d errors captured while the reporter is off", n)
	}
	r.awaitPasses(2)
	if got := r.gh.Requests(); len(got) != 0 {
		t.Fatalf("requests with the reporter off: %+v", got)
	}
	if _, err := os.Stat(r.statePath()); err == nil {
		t.Fatal("the reporter saved state while it is off")
	}
}

func TestErrorReporterFilesEachSourceOnTheConfiguredRepository(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo, `labels = ["bug", "auto"]`), nil)
	before, err := r.c.Statuses(context.Background())
	fixturekit.Must(t, err)
	r.apiError()
	r.opFail("workspace.draft", "draft failed")
	r.githubFail("GET", "/repos/acme/private-repo/pulls/12", 502, "bad gateway")
	got := r.awaitIssues(reportRepo, 3)
	r.quiet(reportRepo, 3)
	sources := map[string]fixturekit.FakeIssue{}
	for _, issue := range got {
		sources[sourceOf(issue.Body)] = issue
		if !slices.Equal(issue.Labels, []string{"bug", "auto"}) {
			t.Fatalf("issue %d labels %v", issue.Number, issue.Labels)
		}
	}
	if len(sources) != 3 {
		t.Fatalf("sources %v in %+v", sources, got)
	}
	for source, want := range map[string]string{"api": "GET /v1/health", "operation": "workspace.draft", "github": "GET /repos/{owner}/{repo}/pulls/{n}"} {
		if issue, ok := sources[source]; !ok || !strings.Contains(issue.Body, want) {
			t.Fatalf("%s issue %+v lacks %q", source, issue, want)
		}
	}

	r.reload(section(reportRepo, `labels = ["bug", "auto"]`, `sources = ["api"]`))
	r.opFail("workspace.seal", "seal failed")
	r.githubFail("GET", "/repos/acme/private-repo/issues/3", 500, "server error")
	r.quiet(reportRepo, 3)

	after, err := r.c.Statuses(context.Background())
	fixturekit.Must(t, err)
	if len(after.Workstreams) != len(before.Workstreams) {
		t.Fatalf("filing issues changed the workstreams: %d then %d", len(before.Workstreams), len(after.Workstreams))
	}
	for _, req := range r.gh.Requests() {
		if !strings.HasPrefix(req.Path, "/repos/"+reportRepo+"/issues") {
			t.Fatalf("the reporter requested %s %s", req.Method, req.Path)
		}
	}
}

// A ghlog Error record for a git remote operation files nothing and is still
// in the GitHub activity log; model-provider turn errors, Jev failures and
// the reporter's own failed GitHub calls file nothing either.
func TestErrorReporterIgnoresWhatItDoesNotReport(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo), func(o *ErrorReporterOptions) { o.Retry = 100 * time.Millisecond })
	r.gitFail()
	r.s.log().Error("agent turn finished", "project", "p_0123456789abcdef", "workstream", "w_0123456789abcdef", "outcome", "failed", "error", "provider overloaded: 529")
	r.s.log().Error("operation failed", "project", "p_0123456789abcdef", "workstream", "w_0123456789abcdef", "action", "thread-turn", "error", "provider overloaded: 529")
	r.s.log().Warn("jev request failed", "component", "jev", "error", "jev is unreachable")
	r.s.log().Error("jev request failed", "component", "jev", "error", "jev is unreachable")
	r.quiet(reportRepo, 0)
	if !strings.Contains(r.log.String(), `"action":"workspace.push"`) || !strings.Contains(r.log.String(), `"remote":"origin"`) {
		t.Fatalf("the git remote failure is missing from the activity log:\n%s", r.log.String())
	}
	if n := len(r.gh.Requests()); n != 0 {
		t.Fatalf("%d requests for records that are not reported", n)
	}

	r.reload(section(reportRepo))
	r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultCreate, Effect: fixturekit.StatusBeforeApply, Status: 500, Times: -1})
	r.opFail("workspace.draft", "draft failed")
	intakefixture.Soon(t, "the failed creates", func() bool { return r.gh.Count("POST", "/repos/"+reportRepo+"/issues") >= 2 })
	r.awaitDiagnostic("retrying")
	if !strings.Contains(r.log.String(), `"action":"issues.create"`) || !strings.Contains(r.log.String(), `"error_reporter":true`) {
		t.Fatalf("the reporter's own calls are missing from the activity log:\n%s", r.log.String())
	}
	r.gh.Clear()
	r.awaitIssues(reportRepo, 1)
	r.awaitResolved()
	r.quiet(reportRepo, 1)
	records := 0
	for _, rs := range r.state().Repositories {
		records += len(rs.Issues)
	}
	if records != 1 {
		t.Fatalf("the reporter's own failures fed back: %+v", r.state().Repositories)
	}
}

func TestErrorReporterGroupsRepeatsAndOpensANewIssueAfterClosing(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo), nil)
	fail := func() { r.opFail("workspace.draft", "draft failed for the unit") }
	fail()
	r.awaitIssues(reportRepo, 1)

	r.clock.Advance(2 * time.Minute)
	fail()
	comments := r.awaitComments(reportRepo, 1, 1)
	if !strings.Contains(comments[0].Body, "Still occurring") || !strings.Contains(comments[0].Body, "2026-11-20") {
		t.Fatalf("comment %q", comments[0].Body)
	}
	r.clock.Advance(2 * time.Minute)
	fail()
	r.quiet(reportRepo, 1)
	if n := len(r.gh.Comments(reportRepo, 1)); n != 1 {
		t.Fatalf("%d comments on one day", n)
	}

	r.clock.Advance(15 * time.Hour)
	fail()
	comments = r.awaitComments(reportRepo, 1, 2)
	if !strings.Contains(comments[1].Body, "2026-11-21") {
		t.Fatalf("second day's comment %q", comments[1].Body)
	}

	r.gh.Close(reportRepo, 1)
	r.clock.Advance(2 * time.Minute)
	fail()
	issues := r.awaitIssues(reportRepo, 2)
	if !strings.Contains(issues[1].Body, "https://github.com/"+reportRepo+"/issues/1") {
		t.Fatalf("the new issue does not link the closed one:\n%s", issues[1].Body)
	}
	if issues[0].State != "closed" {
		t.Fatalf("the closed issue was changed: %+v", issues[0])
	}
	for _, req := range r.gh.Requests() {
		if req.Method != "GET" && req.Method != "POST" {
			t.Fatalf("a %s request was sent: %s", req.Method, req.Path)
		}
	}
	r.quiet(reportRepo, 2)
}

func TestErrorReporterOpensANewIssueForASingleRecurrenceAfterClosing(t *testing.T) {
	t.Parallel()
	t.Run("closed after the days comment", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), nil)
		fail := func() { r.opFail("workspace.draft", "draft failed for the unit") }
		fail()
		r.awaitIssues(reportRepo, 1)
		r.clock.Advance(2 * time.Minute)
		fail()
		r.awaitComments(reportRepo, 1, 1)
		r.gh.Close(reportRepo, 1)

		r.clock.Advance(10 * time.Second)
		fail()
		r.quiet(reportRepo, 1)
		r.clock.Advance(2 * time.Minute)
		issues := r.awaitIssues(reportRepo, 2)
		if !strings.Contains(issues[1].Body, "/issues/1") {
			t.Fatalf("the new issue does not link the closed one:\n%s", issues[1].Body)
		}
	})
	t.Run("comment cap spent", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo, "max_comments_per_day = 1"), nil)
		r.opFail("a.operation", "first failure")
		r.awaitIssues(reportRepo, 1)
		r.clock.Advance(2 * time.Minute)
		r.opFail("a.operation", "first failure")
		r.awaitComments(reportRepo, 1, 1)

		r.opFail("b.operation", "second failure")
		r.awaitIssues(reportRepo, 2)
		r.clock.Advance(2 * time.Minute)
		r.opFail("b.operation", "second failure")
		r.quiet(reportRepo, 2)
		if n := len(r.gh.Comments(reportRepo, 2)); n != 0 {
			t.Fatalf("a comment over the cap: %d", n)
		}
		r.gh.Close(reportRepo, 2)

		r.clock.Advance(10 * time.Second)
		r.opFail("b.operation", "second failure")
		r.quiet(reportRepo, 2)
		r.clock.Advance(2 * time.Minute)
		issues := r.awaitIssues(reportRepo, 3)
		if !strings.Contains(issues[2].Body, "/issues/2") || !strings.Contains(issues[2].Body, "Skipped over a cap:** 1") {
			t.Fatalf("the new issue:\n%s", issues[2].Body)
		}
	})
}

func TestErrorReporterCapsIssuesAndComments(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo, "max_issues_per_hour = 1", "max_issues_per_day = 2", "max_comments_per_day = 1"), nil)
	r.opFail("a.operation", "first failure")
	r.awaitIssues(reportRepo, 1)
	r.opFail("b.operation", "second failure")
	r.quiet(reportRepo, 1)

	r.clock.Advance(61 * time.Minute)
	r.opFail("b.operation", "second failure")
	issues := r.awaitIssues(reportRepo, 2)
	if !strings.Contains(issues[1].Body, "Skipped over a cap:** 1") {
		t.Fatalf("the issue after the cap does not report the skipped count:\n%s", issues[1].Body)
	}

	r.clock.Advance(2 * time.Hour)
	r.opFail("c.operation", "third failure")
	r.quiet(reportRepo, 2)

	r.opFail("a.operation", "first failure")
	r.awaitComments(reportRepo, 1, 1)
	r.opFail("b.operation", "second failure")
	r.quiet(reportRepo, 2)
	if n := len(r.gh.Comments(reportRepo, 2)); n != 0 {
		t.Fatalf("a comment over the cap: %d", n)
	}
	r.clock.Advance(24 * time.Hour)
	r.opFail("b.operation", "second failure")
	comments := r.awaitComments(reportRepo, 2, 1)
	if !strings.Contains(comments[0].Body, "Skipped over a cap:** 1") {
		t.Fatalf("the comment after the cap:\n%s", comments[0].Body)
	}
}

// A failing GitHub leads to retries with a growing delay and a give-up; the
// reporter diagnostic shows in both views while it fails and after it gives up
// and clears after a later success.
func TestErrorReporterRetriesGivesUpAndRecovers(t *testing.T) {
	t.Parallel()
	retry := 40 * time.Millisecond
	r := newReporterRig(t, section(reportRepo), func(o *ErrorReporterOptions) { o.Retry = retry })
	r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultCreate, Effect: fixturekit.StatusBeforeApply, Status: 503, Times: -1})
	r.opFail("workspace.draft", "draft failed")
	r.awaitDiagnostic("retrying")
	create := func() []fixturekit.IssueRequest {
		var out []fixturekit.IssueRequest
		for _, req := range r.gh.Requests() {
			if req.Method == "POST" && req.Path == "/repos/"+reportRepo+"/issues" {
				out = append(out, req)
			}
		}
		return out
	}
	intakefixture.Soon(t, "the reporter to give up", func() bool { return len(create()) >= reportAttempts })
	r.awaitDiagnostic("gave up")
	attempts := create()
	for i := 1; i < len(attempts); i++ {
		if gap, want := attempts[i].At.Sub(attempts[i-1].At), retry<<(i-1)/2; gap < want {
			t.Fatalf("attempt %d came %v after the one before, want at least %v", i+1, gap, want)
		}
	}
	r.awaitPasses(2)
	if n := len(create()); n != reportAttempts {
		t.Fatalf("%d create attempts, want %d and then nothing", n, reportAttempts)
	}
	r.awaitDiagnostic("gave up")

	r.gh.Clear()
	r.opFail("workspace.draft", "draft failed")
	r.awaitIssues(reportRepo, 1)
	r.awaitNoDiagnostic()
	r.quiet(reportRepo, 1)
}

// A report that cannot be made safe to publish files nothing and shows a
// diagnostic, which clears after a later filing.
func TestErrorReporterFailsClosedWhenScrubbingFails(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo), nil)
	r.s.log().Error("operation failed", "project", "not an opaque id!", "workstream", "w_0123456789abcdef", "action", "workspace.draft", "error", "draft failed")
	r.awaitDiagnostic("could not be made safe to publish")
	r.quiet(reportRepo, 0)
	if n := len(r.gh.Requests()); n != 0 {
		t.Fatalf("%d requests after a scrub failure", n)
	}
	r.opFail("workspace.draft", "draft failed")
	r.awaitIssues(reportRepo, 1)
	r.awaitNoDiagnostic()
}

// A missing token shows a diagnostic and sends nothing.
func TestErrorReporterShowsAMissingToken(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo), nil)
	r.tokenOn.Store(false)
	r.reload(section(reportRepo))
	r.awaitDiagnostic("GITHUB_TOKEN is not set")
	r.opFail("workspace.draft", "draft failed")
	r.awaitPasses(2)
	if n := len(r.gh.Requests()); n != 0 {
		t.Fatalf("%d requests without a token", n)
	}
	r.tokenOn.Store(true)
	r.reload(section(reportRepo))
	r.awaitIssues(reportRepo, 1)
	r.awaitNoDiagnostic()
}

// Errors from before the reporter was enabled, or while it was disabled, are
// never filed.
func TestErrorReporterNeverFilesErrorsFromBeforeItWasEnabled(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, "", nil)
	r.opFail("old.operation", "before enabling")
	r.apiError()
	r.reload(section(reportRepo))
	r.opFail("new.operation", "after enabling")
	issues := r.awaitIssues(reportRepo, 1)
	r.quiet(reportRepo, 1)
	if !strings.Contains(issues[0].Title, "new.operation") {
		t.Fatalf("filed %q", issues[0].Title)
	}

	r.tokenOn.Store(false)
	r.reload(section(reportRepo))
	r.opFail("queued.operation", "queued while enabled")
	intakefixture.Soon(t, "the error to be queued", func() bool { return r.s.reporter.capture.queued() == 1 })
	r.reload("")
	if n := r.s.reporter.capture.queued(); n != 0 {
		t.Fatalf("%d errors still queued after disabling", n)
	}
	r.opFail("disabled.operation", "while disabled")
	r.tokenOn.Store(true)
	r.reload(section(reportRepo))
	r.quiet(reportRepo, 1)
}

// Capture never blocks the logging caller: with the worker unable to run and a
// full queue, the oldest errors are dropped and counted, and the next filing
// reports the count.
func TestErrorReporterDropsTheOldestErrorWhenTheQueueIsFull(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, "", func(o *ErrorReporterOptions) { o.Queue = 3 })
	r.tokenOn.Store(false)
	r.reload(section(reportRepo))
	begin := time.Now()
	for i := 0; i < 5; i++ {
		r.opFail(fmt.Sprintf("op%d.operation", i), "failed")
	}
	for i := 0; i < 20000; i++ {
		r.opFail("flood.operation", "failed")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("logging 20005 errors took %v", took)
	}
	c := r.s.reporter.capture
	if n := c.queued(); n != 3 {
		t.Fatalf("%d queued, want the bound of 3", n)
	}
	c.mu.Lock()
	dropped := c.dropped
	oldest := c.queue[0].event.Template
	c.mu.Unlock()
	if dropped != 20002 || oldest != "flood.operation" {
		t.Fatalf("dropped %d, oldest %q", dropped, oldest)
	}
	r.tokenOn.Store(true)
	c.signal()
	issues := r.awaitIssues(reportRepo, 1)
	if !strings.Contains(issues[0].Body, "Skipped from a full queue:** 20002") {
		t.Fatalf("the issue does not report the drops:\n%s", issues[0].Body)
	}
}

func TestErrorReporterKeepsItsIssuesAcrossARestart(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo), nil)
	r.opFail("workspace.draft", "draft failed")
	r.awaitIssues(reportRepo, 1)
	r.awaitResolved()
	if _, err := os.Stat(r.statePath()); err != nil {
		t.Fatal(err)
	}
	cfg := r.s.current()
	state, err := cfg.Root.ErrorReports()
	fixturekit.Must(t, err)
	ledger, err := cfg.Root.Notifications()
	fixturekit.Must(t, err)
	if state != r.statePath() || filepath.Dir(state) != filepath.Dir(ledger) {
		t.Fatalf("state %s is not beside the Notification ledger %s", state, ledger)
	}

	r.restart()
	r.clock.Advance(2 * time.Minute)
	r.opFail("workspace.draft", "draft failed")
	r.awaitComments(reportRepo, 1, 1)
	r.quiet(reportRepo, 1)
}

func TestErrorReporterAdoptsAnIssueWhoseResponseWasLost(t *testing.T) {
	t.Parallel()
	slow := func(o *ErrorReporterOptions) { o.Retry = 300 * time.Millisecond }
	lose := fixturekit.Fault{Kind: fixturekit.FaultCreate, Effect: fixturekit.DropAfterApply}
	t.Run("plain retry", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), nil)
		r.gh.Inject(lose)
		r.opFail("workspace.draft", "draft failed")
		r.awaitIssues(reportRepo, 1)
		r.awaitResolved()
		r.quiet(reportRepo, 1)
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		r.awaitComments(reportRepo, 1, 1)
		r.quiet(reportRepo, 1)
	})
	t.Run("labels changed by reload before the retry", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), slow)
		r.gh.Inject(lose)
		r.opFail("workspace.draft", "draft failed")
		r.awaitIssues(reportRepo, 1)
		r.reload(section(reportRepo, `labels = ["changed"]`))
		r.awaitResolved()
		r.quiet(reportRepo, 1)
		if labels := r.gh.Issues(reportRepo)[0].Labels; !slices.Equal(labels, []string{"factory-error", "automated"}) {
			t.Fatalf("labels %v", labels)
		}
	})
	t.Run("labels removed on the server", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), slow)
		r.gh.Inject(lose)
		r.opFail("workspace.draft", "draft failed")
		r.awaitIssues(reportRepo, 1)
		r.gh.SetLabels(reportRepo, 1)
		r.awaitResolved()
		r.quiet(reportRepo, 1)
		unfiltered := false
		for _, req := range r.gh.Requests() {
			if req.Method == "GET" && req.Path == "/repos/"+reportRepo+"/issues" && !strings.Contains(req.Query, "labels=") && strings.Contains(req.Query, "since=") {
				unfiltered = true
			}
		}
		if !unfiltered {
			t.Fatalf("no lookup without a label filter: %+v", r.gh.Requests())
		}
	})
	t.Run("restart with changed labels before the retry", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), func(o *ErrorReporterOptions) { o.Retry = time.Hour })
		r.gh.Inject(lose)
		r.opFail("workspace.draft", "draft failed")
		r.awaitIssues(reportRepo, 1)
		intakefixture.Soon(t, "the lost response to be noticed", func() bool { return len(r.state().Intents) == 1 })
		r.stop()
		r.files.Write(t, r.files.TopText+section(reportRepo, `labels = ["changed"]`))
		r.launch()
		r.awaitResolved()
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		r.awaitComments(reportRepo, 1, 1)
		r.quiet(reportRepo, 1)
	})
	t.Run("stopped after the server accepted it", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), nil)
		hold := r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultCreate, Effect: fixturekit.HoldAfterApply, Applied: make(chan struct{}, 1), Release: make(chan struct{})})
		r.opFail("workspace.draft", "draft failed")
		<-hold.Applied
		r.stop()
		close(hold.Release)
		if st := r.state(); len(st.Intents) != 1 {
			t.Fatalf("the unresolved write was not kept: %+v", st)
		}
		r.launch()
		r.awaitResolved()
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		r.awaitComments(reportRepo, 1, 1)
		r.quiet(reportRepo, 1)
	})
}

// An accepted comment whose response is lost, or whose result is not saved
// before a restart, leaves one still-occurring comment for the day.
func TestErrorReporterAdoptsACommentWhoseResponseWasLost(t *testing.T) {
	t.Parallel()
	t.Run("response lost", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), nil)
		r.opFail("workspace.draft", "draft failed")
		r.awaitIssues(reportRepo, 1)
		r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultComment, Effect: fixturekit.DropAfterApply})
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		r.awaitComments(reportRepo, 1, 1)
		intakefixture.Soon(t, "the comment to be adopted", func() bool { return len(r.state().Intents) == 0 })
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		r.quiet(reportRepo, 1)
		if n := len(r.gh.Comments(reportRepo, 1)); n != 1 {
			t.Fatalf("%d comments for the day", n)
		}
	})
	t.Run("restart before the result is saved", func(t *testing.T) {
		t.Parallel()
		r := newReporterRig(t, section(reportRepo), nil)
		r.opFail("workspace.draft", "draft failed")
		r.awaitIssues(reportRepo, 1)
		hold := r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultComment, Effect: fixturekit.HoldAfterApply, Applied: make(chan struct{}, 1), Release: make(chan struct{})})
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		<-hold.Applied
		r.stop()
		close(hold.Release)
		r.launch()
		r.awaitResolved()
		r.clock.Advance(2 * time.Minute)
		r.opFail("workspace.draft", "draft failed")
		r.quiet(reportRepo, 1)
		if n := len(r.gh.Comments(reportRepo, 1)); n != 1 {
			t.Fatalf("%d comments for the day", n)
		}
	})
}

func TestErrorReporterScopesIssuesToTheirRepository(t *testing.T) {
	t.Parallel()
	const a, b = "owner/alpha", "owner/beta"
	r := newReporterRig(t, section(a), nil)
	r.opFail("workspace.draft", "draft failed")
	r.awaitIssues(a, 1)
	r.awaitResolved()
	unrelated := r.gh.Seed(b, "an unrelated issue", "not ours")
	if unrelated != 1 {
		t.Fatalf("seeded issue %d", unrelated)
	}

	r.reload(section(b))
	r.clock.Advance(2 * time.Minute)
	r.opFail("workspace.draft", "draft failed")
	issues := r.awaitIssues(b, 2)
	r.quiet(b, 2)
	if strings.Contains(issues[1].Body, "Previous issue") || strings.Contains(issues[1].Body, "/issues/1") {
		t.Fatalf("the new issue links another issue:\n%s", issues[1].Body)
	}
	for _, req := range r.gh.Requests() {
		if strings.HasPrefix(req.Path, "/repos/"+b+"/issues/1") {
			t.Fatalf("the unrelated issue was used: %s %s", req.Method, req.Path)
		}
	}
	if n := len(r.gh.Comments(b, 1)); n != 0 {
		t.Fatalf("%d comments on the unrelated issue", n)
	}

	r.restart()
	r.clock.Advance(2 * time.Minute)
	r.opFail("workspace.draft", "draft failed")
	r.awaitComments(b, 2, 1)

	r.reload(section(a))
	r.clock.Advance(2 * time.Minute)
	r.opFail("workspace.draft", "draft failed")
	r.awaitComments(a, 1, 1)
	r.quiet(a, 1)
	r.quiet(b, 2)
}

// An unresolved write for a repository that is not the target makes no
// further write, and is reconciled when that repository is the target again.
func TestErrorReporterReconcilesAnOldRepositoryWhenItIsTheTargetAgain(t *testing.T) {
	t.Parallel()
	const a, b = "owner/alpha", "owner/beta"
	r := newReporterRig(t, section(a), func(o *ErrorReporterOptions) { o.Retry = time.Hour })
	r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultCreate, Effect: fixturekit.DropAfterApply})
	r.opFail("workspace.draft", "draft failed")
	r.awaitIssues(a, 1)
	intakefixture.Soon(t, "the lost response to be noticed", func() bool { return len(r.state().Intents) == 1 })
	fingerprint := r.state().Intents[0].Fingerprint

	r.reload(section(b))
	r.opFail("workspace.draft", "draft failed")
	r.awaitIssues(b, 1)
	r.quiet(a, 1)
	if got := r.gh.Count("POST", "/repos/"+a+"/issues"); got != 1 {
		t.Fatalf("%d creates in the old repository", got)
	}

	r.reload(section(a))
	r.awaitResolved()
	if rec := r.state().Record(a, fingerprint); rec == nil || rec.Issue != 1 {
		t.Fatalf("the old issue was not adopted: %+v", rec)
	}
	r.quiet(a, 1)
}

func TestErrorReporterFilesNoPrivateValues(t *testing.T) {
	t.Parallel()
	r := newReporterRig(t, section(reportRepo), nil)
	private := []string{"acme/private-repo", "/home/kyle/.local/share/osmia/views", "feature/secret-branch", reportToken, "kyle@example.com", "box.tail1234.ts.net"}
	message := fmt.Sprintf("push of %s from %s on branch %s failed with token %s for %s via https://%s/v1", private[0], private[1], private[2], private[3], private[4], private[5])
	r.githubFail("GET", "/repos/acme/private-repo/pulls/12", 502, message)
	r.opFail("workspace.draft", message)
	issues := r.awaitIssues(reportRepo, 2)
	for _, issue := range issues {
		for _, value := range private {
			if strings.Contains(issue.Title, value) || strings.Contains(issue.Body, value) {
				t.Fatalf("issue %d holds %q:\n%s\n%s", issue.Number, value, issue.Title, issue.Body)
			}
		}
	}
	for _, req := range r.gh.Requests() {
		for _, value := range private {
			if strings.Contains(req.Body, value) {
				t.Fatalf("a request body holds %q: %s", value, req.Body)
			}
		}
	}
}
