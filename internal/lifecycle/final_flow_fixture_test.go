package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	flowTokenEnv = "OSMIA_FLOW_CLOUD_TOKEN"
	flowToken    = "flow-cloud-token"
	flowPoll     = 10 * time.Minute
)

type flowClock struct{ *fixturekit.IssueClock }

func (c *flowClock) Now() time.Time {
	c.Advance(10 * time.Microsecond)
	return c.IssueClock.Now()
}

func (c *flowClock) Peek() time.Time { return c.IssueClock.Now() }

type cloudHost struct {
	mu       sync.Mutex
	clock    *flowClock
	statuses map[string]map[string]pulls.CommitStatus
	reads    map[string][]time.Time
	next     int64
}

func newCloudHost(clock *flowClock) *cloudHost {
	return &cloudHost{clock: clock, statuses: map[string]map[string]pulls.CommitStatus{}, reads: map[string][]time.Time{}}
}

func (h *cloudHost) CommitStatuses(_ context.Context, repository, commit string) (pulls.CommitStatuses, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads[commit] = append(h.reads[commit], h.clock.Peek())
	out := pulls.CommitStatuses{Repository: repository, Commit: commit}
	for _, name := range []string{"load", checkrun.CloudRunContext} {
		if s, ok := h.statuses[commit][name]; ok {
			out.Statuses = append(out.Statuses, s)
		}
	}
	for name, s := range h.statuses[commit] {
		if name != "load" && name != checkrun.CloudRunContext {
			out.Statuses = append(out.Statuses, s)
		}
	}
	return out, nil
}

func (h *cloudHost) readCount(commit string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reads[commit])
}

func (h *cloudHost) totalReads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, reads := range h.reads {
		n += len(reads)
	}
	return n
}

func (h *cloudHost) post(commit string, s pulls.CommitStatus) pulls.CommitStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	s.ID = h.next
	s.CreatedAt = h.clock.Peek()
	s.UpdatedAt = s.CreatedAt
	if h.statuses[commit] == nil {
		h.statuses[commit] = map[string]pulls.CommitStatus{}
	}
	h.statuses[commit][s.Context] = s
	return s
}

func (h *cloudHost) loaded(commit string) pulls.CommitStatus {
	return h.post(commit, pulls.CommitStatus{Context: "load", State: "success"})
}

func (h *cloudHost) pending(commit string) pulls.CommitStatus {
	return h.post(commit, pulls.CommitStatus{Context: checkrun.CloudRunContext, State: "pending"})
}

func (h *cloudHost) completed(commit, state string, passed int) pulls.CommitStatus {
	for i := range passed {
		h.passedCheck(commit, fmt.Sprintf("check-%d", i))
	}
	h.mu.Lock()
	target := fmt.Sprintf("https://dagger.cloud/acme/checks/github.com/acme/widgets@%s?check=%d", commit, h.next+1)
	h.mu.Unlock()
	return h.post(commit, pulls.CommitStatus{Context: checkrun.CloudRunContext, State: state, TargetURL: target})
}

func (h *cloudHost) failedCheck(commit, name, traceID string) pulls.CommitStatus {
	return h.post(commit, pulls.CommitStatus{Context: name, State: "failure", Description: "Failed in 2m33s. Run `dagger trace " + traceID + "`"})
}

func (h *cloudHost) passedCheck(commit, name string) pulls.CommitStatus {
	return h.post(commit, pulls.CommitStatus{Context: name, State: "success", Description: "Passed in 31s."})
}

type countingChecks struct{ calls atomic.Int32 }

func (c *countingChecks) List(context.Context, string, checkrun.Identity) ([]string, error) {
	c.calls.Add(1)
	return nil, nil
}

func (c *countingChecks) Check(context.Context, string, checkrun.Identity, []string) (checkrun.CheckResult, error) {
	c.calls.Add(1)
	return checkrun.CheckResult{Output: "ok"}, nil
}

type flowJourney struct {
	*briefJourney
	clock    *flowClock
	host     *cloudHost
	checks   *countingChecks
	rerunLog string
	traceLog string
	received string
	held     []func()
}

func newFlowJourney(t *testing.T) *flowJourney {
	t.Helper()
	bin := t.TempDir()
	rerunLog := filepath.Join(t.TempDir(), "rerun.log")
	traceLog := filepath.Join(t.TempDir(), "trace.log")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = cloud ] && [ \"$2\" = traces ]; then\n  printf '%%s\\n' \"$*\" >> '%s'\n  echo \"trace output of: $*\"\n  echo \"token: $DAGGER_CLOUD_TOKEN\"\n  exit 0\nfi\n{ printf 'args=%%s\\n' \"$*\"; printf 'pwd=%%s\\n' \"$(pwd -P)\"; printf 'token=%%s\\n' \"$DAGGER_CLOUD_TOKEN\"; } >> '%s'\n", traceLog, rerunLog)
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(flowTokenEnv, flowToken)

	clock := &flowClock{fixturekit.NewIssueClock(fixturekit.DemoStart)}
	b := newBriefFixtureAt(t, "[dagger]\ncloud_api_token_env = \""+flowTokenEnv+"\"\n[events]\nwindow = \"1ms\"\nprogress_window = \"1ms\"\n", clock.Now)
	j := &flowJourney{briefJourney: b, clock: clock, checks: &countingChecks{}, rerunLog: rerunLog, traceLog: traceLog}
	j.host = newCloudHost(j.clock)
	j.Fork, j.Pulls = fixturekit.PublicationFork(t, b.Clone, func() string { commit, _ := j.ForkBranch(t); return commit })
	j.Pulls.SyncHead = true
	j.Pulls.Statuses = j.host.CommitStatuses
	j.received = filepath.Join(filepath.Dir(j.Fork), "received")
	hook := fmt.Sprintf("#!/bin/sh\necho push >> '%s'\n", j.received)
	fixturekit.Must(t, os.WriteFile(filepath.Join(j.Fork, "hooks", "pre-receive"), []byte(hook), 0o700))

	b.Opts.PullRequests = j.Pulls
	b.Opts.FinalChecksWait = nil
	b.Opts.ReviewChecks = j.checks

	stream, repository := b.BuildFixture().SeedGit(t, "flow")
	b.Stream, b.Repository = stream, repository
	b.S.SetSole(&journey.ActiveProject{Repository: repository})
	fixturekit.EditProjectSettings(t, b.Opts.Config.Root, b.Project, func(text string) string { return "checks_timeout = \"2h\"\n" + text })
	journey.MergeDirectly(t, b.ShedFixture, repository, stream, "resume", map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"})
	journey.MergeDirectly(t, b.ShedFixture, repository, stream, "dedupe", map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	return j
}

func (j *flowJourney) start(t *testing.T) {
	t.Helper()
	j.run(t)
	status := j.S.CheckSlots()
	for i := range status.Limit {
		release, err := journey.InspectForTest(j.S).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Project: j.Project, Workstream: j.Stream, Unit: fmt.Sprintf("held-%d", i)})
		fixturekit.Must(t, err)
		j.held = append(j.held, release)
	}
}

func (j *flowJourney) restart(t *testing.T) {
	t.Helper()
	j.halt(t)
	j.held = nil
	j.start(t)
}

func (j *flowJourney) requireNoCheckSlotWait(t *testing.T) {
	t.Helper()
	status := j.S.CheckSlots()
	if len(status.Waiting) != 0 || status.Used != status.Limit {
		t.Fatalf("check slots: %d of %d used, waiting %+v", status.Used, status.Limit, status.Waiting)
	}
}

func (j *flowJourney) live() *trace.Repository { return j.S.Sole().Repository }

func (j *flowJourney) pushes(t *testing.T) []delivery.Push {
	t.Helper()
	pushes, err := delivery.Pushes(j.live(), j.Stream)
	fixturekit.Must(t, err)
	return pushes
}

func (j *flowJourney) awaitPushes(t *testing.T, n int) []delivery.Push {
	t.Helper()
	var pushes []delivery.Push
	intakefixture.Eventually(t, fmt.Sprintf("the %d. push was never recorded", n), func() bool {
		pushes = j.pushes(t)
		return len(pushes) >= n
	})
	return pushes
}

func (j *flowJourney) pushCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(j.received)
	if os.IsNotExist(err) {
		return 0
	}
	fixturekit.Must(t, err)
	return strings.Count(string(data), "push")
}

func (j *flowJourney) rerunCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(j.rerunLog)
	if os.IsNotExist(err) {
		return nil
	}
	fixturekit.Must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "args=")[1:]
}

func (j *flowJourney) traceCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(j.traceLog)
	if os.IsNotExist(err) {
		return nil
	}
	fixturekit.Must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (j *flowJourney) finalOperation(t *testing.T, review int) (trace.OperationRecord, bool) {
	t.Helper()
	ops, err := j.live().Operations(j.Stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action != delivery.FinalReviewAction {
			continue
		}
		if in, err := delivery.DecodeFinalReview(o.Operation); err == nil && in.Review == review {
			return o, true
		}
	}
	return trace.OperationRecord{}, false
}

func (j *flowJourney) lastRetry(t *testing.T, review int) (trace.OperationAction, bool) {
	t.Helper()
	op, found := j.finalOperation(t, review)
	if !found {
		return trace.OperationAction{}, false
	}
	for i := len(op.History) - 1; i >= 0; i-- {
		if op.History[i].Kind == reconcile.RetryAction {
			return op.History[i], true
		}
	}
	return trace.OperationAction{}, false
}

func (j *flowJourney) awaitWaiting(t *testing.T, review int, since time.Time, want string) string {
	t.Helper()
	var last trace.OperationAction
	intakefixture.Eventually(t, fmt.Sprintf("final review %d made no attempt after %s", review, since.Format(time.RFC3339)), func() bool {
		var found bool
		last, found = j.lastRetry(t, review)
		return found && last.At.After(since)
	})
	if !strings.Contains(last.Failure, want) {
		t.Fatalf("final review %d waits for %q, not for %q", review, last.Failure, want)
	}
	return last.Failure
}

func (j *flowJourney) step(t *testing.T, review int, d time.Duration, want string) string {
	t.Helper()
	j.clock.Advance(d)
	return j.awaitWaiting(t, review, j.clock.Peek(), want)
}

func (j *flowJourney) stepTo(t *testing.T, review int, at time.Time, want string) string {
	t.Helper()
	return j.step(t, review, at.Sub(j.clock.Peek()), want)
}

func (j *flowJourney) checkRuns(t *testing.T, review int) []delivery.FinalCheckRun {
	t.Helper()
	var runs []delivery.FinalCheckRun
	for _, d := range intakefixture.StreamDocuments(t, j.live(), j.Stream, delivery.FinalChecksDocument(review)) {
		var run delivery.FinalCheckRun
		fixturekit.Must(t, json.Unmarshal([]byte(d.Content), &run))
		runs = append(runs, run)
	}
	return runs
}

func (j *flowJourney) awaitCheckRun(t *testing.T, review int) delivery.FinalCheckRun {
	t.Helper()
	var run delivery.FinalCheckRun
	intakefixture.Eventually(t, fmt.Sprintf("final review %d recorded no check result", review), func() bool {
		runs := j.checkRuns(t, review)
		if len(runs) == 0 {
			return false
		}
		run = runs[len(runs)-1]
		return run.Status != ""
	})
	return run
}

func (j *flowJourney) awaitReport(t *testing.T, review int) api.FinalReport {
	t.Helper()
	var report api.FinalReport
	intakefixture.Eventually(t, fmt.Sprintf("final review %d recorded no report", review), func() bool {
		r, found, err := delivery.LatestFinalReport(j.live(), j.Stream)
		fixturekit.Must(t, err)
		report = r
		return found && r.Review == review && r.Outcome == delivery.FinalReviewed
	})
	return report
}

func (j *flowJourney) loadMissingNotices(t *testing.T, review, push int) int {
	t.Helper()
	entries, err := j.live().Outbox(j.Stream)
	fixturekit.Must(t, err)
	n := 0
	for _, e := range entries {
		if e.Event.ID == trace.EventID(fmt.Sprintf("final-checks-%d-push-%d-load-missing", review, push), trace.ManagerKey) {
			n++
		}
	}
	return n
}

func (j *flowJourney) feature(t *testing.T) string {
	t.Helper()
	state, err := j.live().Workflow(j.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	return state.Value
}

func (j *flowJourney) awaitHeard(t *testing.T, text string) {
	t.Helper()
	intakefixture.Eventually(t, "the chief of staff never heard "+text, func() bool { return j.manager.heardOf(text) > 0 })
}
