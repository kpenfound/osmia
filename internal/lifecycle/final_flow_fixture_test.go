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
	// flowTokenEnv names the environment variable the project's cloud_api_token_env
	// points at, and flowToken is what it holds.
	flowTokenEnv = "OSMIA_FLOW_CLOUD_TOKEN"
	flowToken    = "flow-cloud-token"
	// flowPoll is the default pull_request_poll, which the journey advances
	// its clock by between two status reads.
	flowPoll = 10 * time.Minute
)

// flowClock is the service's clock in the draft-first journey. It moves a few
// microseconds on every read, so that records keep their order, and otherwise
// only when the journey advances it.
type flowClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *flowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(10 * time.Microsecond)
	return c.at
}

// Peek reads the clock without moving it.
func (c *flowClock) Peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *flowClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// cloudHost stands in for GitHub's combined commit statuses. It shows the
// latest status of each context of a commit, as GitHub does, and keeps every
// read it answers.
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

// readCount is how many times commit's statuses were read.
func (h *cloudHost) readCount(commit string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reads[commit])
}

// totalReads is how many status reads the host answered for any commit.
func (h *cloudHost) totalReads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, reads := range h.reads {
		n += len(reads)
	}
	return n
}

// post makes status the latest of its context on commit, as a new posting
// created now, and returns it.
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

// completed posts passed successful per-check statuses and then a finished
// "Run Dagger Checks" status in state, whose link is a Cloud page.
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

// countingChecks is the unit check runner of the journey, which no unit check
// of the workstream reaches; it counts the lists and runs it is asked for.
type countingChecks struct{ calls atomic.Int32 }

func (c *countingChecks) List(context.Context, string, checkrun.Identity) ([]string, error) {
	c.calls.Add(1)
	return nil, nil
}

func (c *countingChecks) Check(context.Context, string, checkrun.Identity, []string) (checkrun.CheckResult, error) {
	c.calls.Add(1)
	return checkrun.CheckResult{Output: "ok"}, nil
}

// flowJourney is the brief journey's project with a pull request host that
// shows Dagger Cloud statuses, a fork that counts the pushes it receives, a
// `dagger` on the search path that logs the calls it gets, a clock the journey
// moves and a workstream whose two units have merged. It holds every check
// slot, so that a final stage that waited for one would never finish.
type flowJourney struct {
	*briefJourney
	clock    *flowClock
	host     *cloudHost
	checks   *countingChecks
	rerunLog string
	// traceLog holds the calls the fake `dagger` got that fetch a trace.
	traceLog string
	received string
	held     []func()
}

// newFlowJourney builds the journey's fixture and merges both units of its
// workstream onto the feature branch with the service stopped. The test must
// not be parallel: the fake `dagger` and the token reach the service through
// the process environment.
func newFlowJourney(t *testing.T) *flowJourney {
	t.Helper()
	bin := t.TempDir()
	rerunLog := filepath.Join(t.TempDir(), "rerun.log")
	traceLog := filepath.Join(t.TempDir(), "trace.log")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = cloud ] && [ \"$2\" = traces ]; then\n  printf '%%s\\n' \"$*\" >> '%s'\n  echo \"trace output of: $*\"\n  echo \"token: $DAGGER_CLOUD_TOKEN\"\n  exit 0\nfi\n{ printf 'args=%%s\\n' \"$*\"; printf 'pwd=%%s\\n' \"$(pwd -P)\"; printf 'token=%%s\\n' \"$DAGGER_CLOUD_TOKEN\"; } >> '%s'\n", traceLog, rerunLog)
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(flowTokenEnv, flowToken)

	clock := &flowClock{at: demoStart}
	b := newBriefFixtureAt(t, "[dagger]\ncloud_api_token_env = \""+flowTokenEnv+"\"\n[events]\nwindow = \"1ms\"\nprogress_window = \"1ms\"\n", clock.Now)
	j := &flowJourney{briefJourney: b, clock: clock, checks: &countingChecks{}, rerunLog: rerunLog, traceLog: traceLog}
	j.host = newCloudHost(j.clock)
	j.fork, j.pulls = fixturekit.PublicationFork(t, b.clone, func() string { commit, _ := j.forkBranch(t); return commit })
	j.pulls.SyncHead = true
	j.pulls.Statuses = j.host.CommitStatuses
	j.received = filepath.Join(filepath.Dir(j.fork), "received")
	hook := fmt.Sprintf("#!/bin/sh\necho push >> '%s'\n", j.received)
	must(t, os.WriteFile(filepath.Join(j.fork, "hooks", "pre-receive"), []byte(hook), 0o700))

	// The service reads statuses through the pull request host it publishes
	// with and waits the whole 2 minutes after a push.
	b.opts.PullRequests = j.pulls
	b.opts.FinalChecksWait = nil
	b.opts.reviewChecks = j.checks

	stream, repository := b.buildFixture().SeedGit(t, "flow")
	b.stream, b.repository = stream, repository
	b.s.setSole(&activeProject{repository: repository})
	fixturekit.EditProjectSettings(t, b.opts.Config.Root, b.project, func(text string) string { return "checks_timeout = \"2h\"\n" + text })
	mergeDirectly(t, b.shedFixture, repository, stream, "resume", map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"})
	mergeDirectly(t, b.shedFixture, repository, stream, "dedupe", map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	return j
}

// start runs the service on the trace the journey holds open and takes every
// check slot.
func (j *flowJourney) start(t *testing.T) {
	t.Helper()
	j.run(t)
	status := j.s.CheckSlots()
	for i := range status.Limit {
		release, err := InspectForTest(j.s).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Project: j.project, Workstream: j.stream, Unit: fmt.Sprintf("held-%d", i)})
		must(t, err)
		j.held = append(j.held, release)
	}
}

// restart stops the service and starts it again on the same trace, as the
// owner restarting it would.
func (j *flowJourney) restart(t *testing.T) {
	t.Helper()
	j.halt(t)
	j.held = nil
	j.start(t)
}

// requireNoCheckSlotWait fails the test when anything waits for a check slot.
func (j *flowJourney) requireNoCheckSlotWait(t *testing.T) {
	t.Helper()
	status := j.s.CheckSlots()
	if len(status.Waiting) != 0 || status.Used != status.Limit {
		t.Fatalf("check slots: %d of %d used, waiting %+v", status.Used, status.Limit, status.Waiting)
	}
}

// live is the service's open trace.
func (j *flowJourney) live() *trace.Repository { return j.s.sole().repository }

func (j *flowJourney) pushes(t *testing.T) []delivery.Push {
	t.Helper()
	pushes, err := delivery.Pushes(j.live(), j.stream)
	must(t, err)
	return pushes
}

// awaitPushes waits until n pushes are recorded and returns them.
func (j *flowJourney) awaitPushes(t *testing.T, n int) []delivery.Push {
	t.Helper()
	var pushes []delivery.Push
	intakefixture.Eventually(t, fmt.Sprintf("the %d. push was never recorded", n), func() bool {
		pushes = j.pushes(t)
		return len(pushes) >= n
	})
	return pushes
}

// pushCount is how many pushes the fork has received.
func (j *flowJourney) pushCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(j.received)
	if os.IsNotExist(err) {
		return 0
	}
	must(t, err)
	return strings.Count(string(data), "push")
}

// rerunCalls returns the calls the fake `dagger` got, one entry per call.
func (j *flowJourney) rerunCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(j.rerunLog)
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "args=")[1:]
}

// traceCalls returns the trace fetches the fake `dagger` got, one entry per call.
func (j *flowJourney) traceCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(j.traceLog)
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// finalOperation returns final review k's operation.
func (j *flowJourney) finalOperation(t *testing.T, review int) (trace.OperationRecord, bool) {
	t.Helper()
	ops, err := j.live().Operations(j.stream)
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action != FinalReviewAction {
			continue
		}
		if in, err := decodeFinalReview(o.Operation); err == nil && in.Review == review {
			return o, true
		}
	}
	return trace.OperationRecord{}, false
}

// lastRetry is final review k's latest failed attempt.
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

// awaitWaiting waits until final review k has failed an attempt for lack of
// checks, and returns why. It fails when the reason lacks want.
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

// step moves the clock by d, which makes the waiting final review's next
// attempt due, and returns why the attempt it waits for gives up again.
func (j *flowJourney) step(t *testing.T, review int, d time.Duration, want string) string {
	t.Helper()
	j.clock.Advance(d)
	return j.awaitWaiting(t, review, j.clock.Peek(), want)
}

// stepTo moves the clock to at, which makes the waiting final review's next
// attempt due, and returns why the attempt gives up again.
func (j *flowJourney) stepTo(t *testing.T, review int, at time.Time, want string) string {
	t.Helper()
	return j.step(t, review, at.Sub(j.clock.Peek()), want)
}

// checkRuns returns the check records of final review k, oldest first.
func (j *flowJourney) checkRuns(t *testing.T, review int) []delivery.FinalCheckRun {
	t.Helper()
	var runs []delivery.FinalCheckRun
	for _, d := range streamDocuments(t, j.live(), j.stream, delivery.FinalChecksDocument(review)) {
		var run delivery.FinalCheckRun
		must(t, json.Unmarshal([]byte(d.Content), &run))
		runs = append(runs, run)
	}
	return runs
}

// awaitCheckRun waits for the completed record of final review k.
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

// awaitReport waits for final review k's recorded report.
func (j *flowJourney) awaitReport(t *testing.T, review int) FinalReport {
	t.Helper()
	var report FinalReport
	intakefixture.Eventually(t, fmt.Sprintf("final review %d recorded no report", review), func() bool {
		r, found, err := latestFinalReport(j.live(), j.stream)
		must(t, err)
		report = r
		return found && r.Review == review && r.Outcome == finalReviewed
	})
	return report
}

// loadMissingNotices counts the chief of staff's notices that Dagger Cloud
// posted no `load` status for a push.
func (j *flowJourney) loadMissingNotices(t *testing.T, review, push int) int {
	t.Helper()
	entries, err := j.live().Outbox(j.stream)
	must(t, err)
	n := 0
	for _, e := range entries {
		if e.Event.ID == trace.EventID(fmt.Sprintf("final-checks-%d-push-%d-load-missing", review, push), "chief") {
			n++
		}
	}
	return n
}

// feature is the workstream's feature state.
func (j *flowJourney) feature(t *testing.T) string {
	t.Helper()
	state, err := j.live().Workflow(j.stream, trace.FeatureSubject)
	must(t, err)
	return state.Value
}

// awaitHeard waits until the chief of staff has handled a prompt that holds
// text, and fails with what it did handle otherwise.
func (j *flowJourney) awaitHeard(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for j.chief.heardOf(text) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the chief of staff never heard %q; it heard %q", text, j.chief.prompts())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
