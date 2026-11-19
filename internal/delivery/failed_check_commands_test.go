package delivery

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/pulls"
)

const (
	traceA    = "4e3a3896a86e28f2204157939fb07b9f"
	traceB    = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"
	traceLoad = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

// cloudFailedStatus is the commit status Dagger Cloud posts for a failed
// check: its own description naming the trace, and its own link.
func cloudFailedStatus(f *watchFixture, id int64, name, traceID string, at time.Duration) pulls.CommitStatus {
	s := pulls.CommitStatus{ID: id, Context: name, State: "failure", CreatedAt: f.at(at),
		TargetURL: "https://dagger.cloud/acme/checks/github.com/acme/widgets@" + pushedCommit + "?check=" + name}
	if traceID != "" {
		s.Description = "Failed in 6m17s. Run `dagger trace " + traceID + "` to view the full trace."
	} else {
		s.Description = "Failed in 6m17s."
	}
	return s
}

// settleFailedRun reads the statuses of a failed run, lets the once-only
// rerun come back with the same failure, and returns the recorded result.
func settleFailedRun(f *watchFixture, countsJSON string, failing ...func(at time.Duration) pulls.CommitStatus) FinalCheckRun {
	f.t.Helper()
	post := func(id int64, at time.Duration) {
		statuses := []pulls.CommitStatus{f.loaded(), f.done(id, "failure", at, countsJSON)}
		for _, status := range failing {
			statuses = append(statuses, status(at))
		}
		f.api.set(statuses...)
	}
	f.afterWait()
	post(10, 100*time.Second)
	f.mustWait()
	f.clock.Advance(10 * time.Minute)
	post(20, f.clock.Now().Sub(f.pushedAt))
	return f.mustComplete()
}

func TestFailedCloudRunGivesEverySurfaceEachFailedCheckWithItsTraceCommand(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	var n int64 = 30
	check := func(name, trace string) func(time.Duration) pulls.CommitStatus {
		n++
		id := n
		return func(at time.Duration) pulls.CommitStatus { return cloudFailedStatus(f, id, name, trace, at) }
	}
	run := settleFailedRun(f, counts(92, 89, 3, 0), check("test:a", traceA), check("build", traceB), check("lint", ""))
	if run.Status != "failed" || len(run.FailedChecks) != 3 || len(f.rerunCalls()) != 1 {
		t.Fatalf("run %+v, reruns %q", run, f.rerunCalls())
	}

	// The durable record keeps each failed check's name and trace id.
	recorded := f.runs()
	last := recorded[len(recorded)-1]
	traces := map[string]string{}
	for _, c := range last.FailedChecks {
		traces[c.Name] = c.TraceID
	}
	if traces["test:a"] != traceA || traces["build"] != traceB || traces["lint"] != "" || len(traces) != 3 {
		t.Fatalf("recorded failed checks %+v", last.FailedChecks)
	}

	lint := "- lint: Dagger Cloud posted no trace id for this check; status: https://dagger.cloud/acme/checks/github.com/acme/widgets@" + pushedCommit + "?check=lint"
	surfaces := map[string]string{
		"reader evidence": FinalCheckEvidence(last),
		"chief notice":    f.noticeOf(),
	}
	for name, text := range surfaces {
		for _, want := range []string{"- test:a: `dagger trace " + traceA + "`", "- build: `dagger trace " + traceB + "`", lint} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, text)
			}
		}
	}

	// The poll reads the same failure afresh.
	got := f.call(ChecksToolOf(f.load(), f.repo, f.chief("poll")), `{"action":"poll","push":1}`)
	if got["outcome"] != "failed" {
		t.Fatalf("poll %v", got)
	}
	listed, _ := got["failed_checks"].([]any)
	commands := map[string]string{}
	for _, item := range listed {
		c := item.(map[string]any)
		commands[c["name"].(string)], _ = c["command"].(string)
		if c["name"] == "test:a" && c["trace_id"] != traceA {
			t.Errorf("poll check %v", c)
		}
	}
	if len(listed) != 3 || commands["test:a"] != "dagger trace "+traceA || commands["build"] != "dagger trace "+traceB || commands["lint"] != "" {
		t.Fatalf("poll failed checks %v", listed)
	}
	detail, _ := got["detail"].(string)
	for _, want := range []string{"`dagger trace " + traceA + "`", "`dagger trace " + traceB + "`", "lint: Dagger Cloud posted no trace id"} {
		if !strings.Contains(detail, want) {
			t.Errorf("poll detail lacks %q:\n%s", want, detail)
		}
	}
}

// noticeOf is the failed-check part of the chief's notice for the review the
// fixture's watch recorded a result for.
func (f *watchFixture) noticeOf() string {
	f.t.Helper()
	notice, err := f.reviewer.failedChecksNotice(stream, f.report)
	if err != nil {
		f.t.Fatal(err)
	}
	return notice
}

func TestFailedLoadIsTheFailedCheckLoadOnEverySurface(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	f.afterWait()
	f.api.set(pulls.CommitStatus{ID: 1, Context: "load", State: "failure", Description: "Failed in 12s. Run `dagger trace " + traceLoad + "` to view the full trace.", CreatedAt: f.at(30 * time.Second)})
	run := f.mustComplete()
	want := "- load: `dagger trace " + traceLoad + "`"
	if run.Status != "failed" || len(f.runs()) != 1 || f.runs()[0].FailedChecks[0].Name != "load" || f.runs()[0].FailedChecks[0].TraceID != traceLoad {
		t.Fatalf("run %+v", run)
	}
	if evidence, notice := FinalCheckEvidence(run), f.noticeOf(); !strings.Contains(evidence, want) || !strings.Contains(notice, want) {
		t.Fatalf("evidence:\n%s\nnotice:\n%s", evidence, notice)
	}
	got := f.call(ChecksToolOf(f.load(), f.repo, f.chief("poll")), `{"action":"poll","push":1}`)
	listed, _ := got["failed_checks"].([]any)
	if got["outcome"] != "load_failed" || len(listed) != 1 || listed[0].(map[string]any)["command"] != "dagger trace "+traceLoad {
		t.Fatalf("poll %v", got)
	}
}

func TestFailedRunWithoutAFailingCheckStatusSaysSoWithTheCountsAndTheRunURL(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	run := settleFailedRun(f, counts(92, 91, 1, 0))
	if run.Status != "failed" || len(run.FailedChecks) != 0 {
		t.Fatalf("run %+v", run)
	}
	want := fmt.Sprintf("no failing check status was found, so no failed check can be named: the run's JSON counts 1 failed of 92 checks (91 passed, 0 cancelled); run status: %s", run.StatusURL)
	got := f.call(ChecksToolOf(f.load(), f.repo, f.chief("poll")), `{"action":"poll","push":1}`)
	detail, _ := got["detail"].(string)
	if _, listed := got["failed_checks"]; listed || got["outcome"] != "failed" || !strings.Contains(detail, want) {
		t.Fatalf("poll %v", got)
	}
	for name, text := range map[string]string{"reader evidence": FinalCheckEvidence(run), "chief notice": f.noticeOf()} {
		if !strings.Contains(text, want) || strings.Contains(text, "Failed checks (") {
			t.Errorf("%s:\n%s", name, text)
		}
	}
}

func TestPassedAndIncompleteRunsGiveNoFailedChecks(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.api.set(f.loaded(), f.done(3, "success", 100*time.Second, counts(92, 92, 0, 0)))
	run := f.mustComplete()
	if run.Status != "passed" || strings.Contains(FinalCheckEvidence(run), "Failed") || f.noticeOf() != "" {
		t.Fatalf("passed run %+v:\n%s", run, FinalCheckEvidence(run))
	}

	f = newWatchFixture(t)
	f.cfg.Project.ChecksTimeout = "30m"
	f.afterWait()
	f.api.set(f.loaded(), f.done(3, "success", 100*time.Second, counts(92, 90, 0, 2)))
	if _, err := f.check(); err == nil {
		t.Fatal("an incomplete run settled")
	}
	runs := f.runs()
	if len(runs) != 1 || runs[0].Status != ChecksIncomplete || len(runs[0].FailedChecks) != 0 || strings.Contains(FinalCheckEvidence(runs[0]), "no failing check status") || f.noticeOf() != "" {
		t.Fatalf("incomplete runs %+v", runs)
	}
}
