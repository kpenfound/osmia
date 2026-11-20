package delivery

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
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
func settleFailedRun(f *watchFixture, passed int, failing ...func(at time.Duration) pulls.CommitStatus) FinalCheckRun {
	f.t.Helper()
	post := func(id int64, at time.Duration) {
		statuses := []pulls.CommitStatus{f.loaded(), f.done(id, "failure", at)}
		for _, status := range failing {
			statuses = append(statuses, status(at))
		}
		f.api.setWithChecks(passed, statuses...)
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
	run := settleFailedRun(f, 89, check("test:a", traceA), check("build", traceB), check("lint", ""))
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
	run := settleFailedRun(f, 91)
	if run.Status != "failed" || len(run.FailedChecks) != 0 {
		t.Fatalf("run %+v", run)
	}
	want := fmt.Sprintf("no failing check status was found, so no failed check can be named: the run's check statuses show 91 checks, 91 passed, 0 failed; run status: %s", run.StatusURL)
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
	f.api.setWithChecks(92, f.loaded(), f.done(3, "success", 100*time.Second))
	run := f.mustComplete()
	if run.Status != "passed" || strings.Contains(FinalCheckEvidence(run), "Failed") || f.noticeOf() != "" {
		t.Fatalf("passed run %+v:\n%s", run, FinalCheckEvidence(run))
	}

	f = newWatchFixture(t)
	f.cfg.Project.ChecksTimeout = "30m"
	f.afterWait()
	f.api.setWithChecks(90, f.loaded(), f.done(3, "success", 100*time.Second), pulls.CommitStatus{ID: 70, Context: "slow-a", State: "pending"}, pulls.CommitStatus{ID: 71, Context: "slow-b", State: "pending"})
	if _, err := f.check(); err == nil {
		t.Fatal("an incomplete run settled")
	}
	runs := f.runs()
	if len(runs) != 1 || runs[0].Status != ChecksIncomplete || len(runs[0].FailedChecks) != 0 || strings.Contains(FinalCheckEvidence(runs[0]), "no failing check status") || f.noticeOf() != "" {
		t.Fatalf("incomplete runs %+v", runs)
	}
}

// TestFailedRunLinksCarryNoCredentialOnAnySurface drives a failed run whose
// status links carry credentials through the watch, the reader's evidence,
// the chief's notice and the poll tool.
func TestFailedRunLinksCarryNoCredentialOnAnySurface(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.assemble()
	const secret = "s3cretvalue"
	runURL := "https://dagger.cloud/run.json?token=" + secret
	post := func(id int64, at time.Duration) {
		done := pulls.CommitStatus{ID: id, Context: "Run Dagger Checks", State: "failure", CreatedAt: f.at(at), TargetURL: runURL}
		withTrace := cloudFailedStatus(f, id+100, "test:a", traceA, at)
		withTrace.TargetURL += "&access_token=" + secret
		userinfo := cloudFailedStatus(f, id+101, "build", "", at)
		userinfo.TargetURL = "https://user:" + secret + "@dagger.cloud/acme/checks?check=build"
		queried := cloudFailedStatus(f, id+102, "lint", "", at)
		queried.TargetURL += "&sig=" + secret
		withKey := cloudFailedStatus(f, id+103, "docs", traceB, at)
		f.api.set(f.loaded(), done, withTrace, userinfo, queried, withKey)
	}
	f.afterWait()
	post(10, 100*time.Second)
	f.mustWait()
	f.clock.Advance(10 * time.Minute)
	post(20, f.clock.Now().Sub(f.pushedAt))
	run := f.mustComplete()
	if run.Status != "failed" || len(run.FailedChecks) != 4 || run.StatusURL != runURL {
		t.Fatalf("run %+v", run)
	}

	poll := f.call(ChecksToolOf(f.load(), f.repo, f.chief("poll")), `{"action":"poll","push":1}`)
	pollText, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	surfaces := map[string]string{
		"reader evidence": FinalCheckEvidence(run),
		"chief notice":    f.noticeOf(),
		"poll result":     string(pollText),
	}
	for name, text := range surfaces {
		if strings.Contains(text, secret) || strings.Contains(text, "user:") {
			t.Errorf("%s carries a credential:\n%s", name, text)
		}
		for _, want := range []string{"dagger trace " + traceA, "dagger trace " + traceB, "build", "lint"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, text)
			}
		}
	}
	if evidence := surfaces["reader evidence"]; !strings.Contains(evidence, "\nStatus: https://dagger.cloud/run.json\n") {
		t.Errorf("reader evidence lacks the run's status link without its credential:\n%s", evidence)
	}
	if strings.Contains(surfaces["chief notice"], "run status:") || !strings.Contains(surfaces["chief notice"], "lint: Dagger Cloud posted no trace id for this check; status: https://dagger.cloud/acme/checks/github.com/acme/widgets@"+pushedCommit+"?check=lint") {
		t.Errorf("chief notice:\n%s", surfaces["chief notice"])
	}

	// The durable record is unchanged by what is shown.
	recorded := f.runs()
	if last := recorded[len(recorded)-1]; last.StatusURL != runURL || len(last.FailedChecks) != 4 {
		t.Errorf("recorded run %+v", last)
	}
}

func TestStatusURLWithUserinfoIsNotShownToAnyReader(t *testing.T) {
	t.Parallel()
	run := FinalCheckRun{Source: api.CheckSourcePullRequest, Status: "failed", Review: 1, PullRequest: 7, Push: 1, PushedCommit: pushedCommit,
		StatusURL: "https://user:s3cretvalue@dagger.cloud/run.json", Error: "read https://dagger.cloud/run.json?token=s3cretvalue failed",
		Counts:       &api.CheckCounts{Total: 3, Passed: 2, Failed: 1},
		FailedChecks: []api.CheckFailure{{Name: "lint", TraceID: traceA}}}
	for name, text := range map[string]string{"evidence": FinalCheckEvidence(run), "notice": FailedChecksNotice(run)} {
		if strings.Contains(text, "s3cretvalue") || strings.Contains(text, "user:") || !strings.Contains(text, "dagger trace "+traceA) {
			t.Errorf("%s:\n%s", name, text)
		}
	}
	if evidence := FinalCheckEvidence(run); strings.Contains(evidence, "Status:") || !strings.Contains(evidence, "Note: read https://dagger.cloud/run.json failed") {
		t.Errorf("evidence:\n%s", evidence)
	}
}
