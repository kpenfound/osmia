package delivery

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/pulls"
)

func traceRefs(evidence api.CheckEvidence) map[string]api.CheckTraceRef {
	out := map[string]api.CheckTraceRef{}
	for _, tr := range evidence.Traces {
		out[tr.Kind+" "+tr.Check] = tr
	}
	return out
}

func TestFinalChecksRecordTheCloudEvidenceOfAFlakyCheck(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	f.afterWait()
	f.failingRun()
	f.mustWait()
	f.clock.Advance(10 * time.Minute)
	fresh := f.done(14, "success", f.clock.Now().Sub(f.pushedAt))
	at := f.clock.Now()
	f.api.set(f.loaded(), fresh,
		pulls.CommitStatus{ID: 15, Context: "test:a", State: "success", Description: "Passed in 1m1s. Run `dagger trace fresh-a`", CreatedAt: at},
		pulls.CommitStatus{ID: 16, Context: "test:b", State: "success", Description: "Passed in 40s.", TargetURL: "https://dagger.cloud/traces/fresh-b", CreatedAt: at},
		pulls.CommitStatus{ID: 17, Context: "lint", State: "success", CreatedAt: at})
	run := f.mustComplete()
	if run.Status != "passed" || run.ExitCode != 0 || len(run.Flaky) != 2 || len(run.Failed) != 0 || run.Counts == nil || run.Counts.Total != 3 {
		t.Fatalf("the added capture changed the result: %+v", run)
	}
	stored := f.runs()
	got := stored[len(stored)-1].Evidence
	if got == nil {
		t.Fatalf("the stored record holds no evidence: %+v", stored[len(stored)-1])
	}
	if got.Engine != api.CheckEngineCloud || got.Parallelism != 0 || got.OutputPath != "" {
		t.Fatalf("evidence %+v", got)
	}
	if want := []string{"lint", "test:a", "test:b"}; !slices.Equal(got.CoRun, want) {
		t.Fatalf("co-run %v, want every check reported for the commit %v", got.CoRun, want)
	}
	traces := traceRefs(*got)
	if traces["failed test:a"].TraceID != "trace-a" || traces["failed test:b"].TraceID != "trace-b" || traces["rerun test:a"].TraceID != "fresh-a" || traces["rerun test:b"].URL != "https://dagger.cloud/traces/fresh-b" || traces["run "].URL != fresh.TargetURL {
		t.Fatalf("traces %+v", got.Traces)
	}
	if want := []api.CheckTiming{{Check: "test:a", State: "passed", Duration: "1m1s"}, {Check: "test:b", State: "passed", Duration: "40s"}}; !slices.Equal(got.Timings, want) {
		t.Fatalf("timings %+v", got.Timings)
	}
	items := map[string]api.CheckUnavailable{}
	for _, u := range got.Unavailable {
		items[u.Item] = u
	}
	for _, item := range []string{api.CheckItemOutput, api.CheckItemParallelism} {
		u, ok := items[item]
		if !ok || u.Engine != api.CheckEngineCloud || !strings.Contains(u.Where, "dagger trace trace-a") || !strings.Contains(u.Where, "dagger trace fresh-a") {
			t.Fatalf("%s is not marked unavailable with its trace: %+v", item, got.Unavailable)
		}
	}
	converted := run.FlakeEvidence()
	if converted.Engine != api.CheckEngineCloud || !slices.Equal(converted.Flaky, []string{"test:a", "test:b"}) || !slices.Equal(converted.CoRun, got.CoRun) {
		t.Fatalf("converted %+v", converted)
	}
}

func TestFinalChecksRecordCloudEvidenceOnFailedAndIncompleteRunsWithoutChangingThem(t *testing.T) {
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
	if run.Status != "failed" || run.ExitCode != 1 || len(run.Failed) != 1 || run.Evidence == nil || run.Evidence.Engine != api.CheckEngineCloud {
		t.Fatalf("failed run %+v", run)
	}
	if want := []string{"test:a", "test:b"}; !slices.Equal(run.Evidence.CoRun, want) {
		t.Fatalf("co-run %v", run.Evidence.CoRun)
	}
	var failedB []string
	for _, tr := range run.Evidence.Traces {
		if tr.Kind == api.CheckTraceFailed && tr.Check == "test:b" {
			failedB = append(failedB, tr.TraceID)
		}
	}
	traces := traceRefs(*run.Evidence)
	if !slices.Equal(failedB, []string{"trace-b", "trace-b2"}) || traces["failed test:a"].TraceID != "trace-a" || traces["rerun test:a"].Check != "test:a" || len(run.Flaky) != 1 {
		t.Fatalf("traces %+v", run.Evidence.Traces)
	}

	g := newWatchFixture(t)
	g.afterWait()
	g.api.setWithChecks(8, g.loaded(), g.done(3, "success", 5*time.Minute), pulls.CommitStatus{ID: 70, Context: "slow-a", State: "pending"}, pulls.CommitStatus{ID: 71, Context: "slow-b", State: "pending"})
	incomplete, err := g.check()
	if err == nil || incomplete.Status != ChecksIncomplete || incomplete.ExitCode != -1 || incomplete.Evidence == nil || incomplete.Evidence.Engine != api.CheckEngineCloud {
		t.Fatalf("incomplete run %+v: %v", incomplete, err)
	}
}

func TestPullRequestRunWithoutRecordedEvidenceConvertsFromItsFailuresAndReruns(t *testing.T) {
	t.Parallel()
	recorded := `{"review":1,"operation":"review","commit":"` + reviewedCommit + `","source":"pull_request","pull_request":7,"push":1,"pushed_commit":"` + pushedCommit + `","status":"passed","status_url":"https://dagger.cloud/checks/14.json","counts":{"total":2,"passed":2,"failed":0,"cancelled":0},` +
		`"reruns":[{"requested_at":"2026-01-02T03:04:05Z","original":{"id":10,"created_at":"2026-01-02T03:00:00Z"},"failed":[{"name":"test:a","trace_id":"trace-a","status_id":11}],"flakes":["test:a"]}],"flaky":["test:a"],"started_at":"2026-01-02T03:04:05Z","finished_at":"2026-01-02T03:05:05Z"}`
	var run FinalCheckRun
	if err := json.Unmarshal([]byte(recorded), &run); err != nil {
		t.Fatal(err)
	}
	evidence := run.FlakeEvidence()
	if evidence.Engine != api.CheckEngineCloud || !slices.Equal(evidence.CoRun, []string{"test:a"}) || !slices.Equal(evidence.Flaky, []string{"test:a"}) {
		t.Fatalf("evidence %+v", evidence)
	}
	if traceRefs(evidence)["failed test:a"].TraceID != "trace-a" || len(evidence.Unavailable) != 2 || evidence.Unavailable[0].Where != "dagger trace trace-a" {
		t.Fatalf("evidence %+v", evidence)
	}
}

func TestLocalRunWithoutRecordedEvidenceConvertsFromItsSelectionAndOutput(t *testing.T) {
	t.Parallel()
	local := FinalCheckRun{Review: 1, Status: "passed", Flaky: []string{"dag://y"}, Selection: &CheckSelection{Links: []string{"dag://y", "dag://z"}}, OutputPath: "final/checks-1-output.txt", OutputRevision: 1}.FlakeEvidence()
	if local.Engine != api.CheckEngineLocal || !slices.Equal(local.CoRun, []string{"dag://y", "dag://z"}) || local.OutputPath == "" || len(local.Unavailable) == 0 || local.Unavailable[0].Where != FinalChecksPath(1) {
		t.Fatalf("local %+v", local)
	}
}

func TestAFinalCheckRecordWithCancelledCountsRendersAsEvidence(t *testing.T) {
	t.Parallel()
	recorded := `{"review":1,"operation":"review","commit":"` + reviewedCommit + `","source":"pull_request","pull_request":7,"push":1,"pushed_commit":"` + pushedCommit + `","status":"incomplete","exit_code":-1,` +
		`"error":"the completed run is not a pass: 1 of 10 checks were cancelled","status_url":"https://dagger.cloud/checks/3.json","counts":{"total":10,"passed":9,"failed":0,"cancelled":1},"started_at":"2026-01-02T03:04:05Z","finished_at":"2026-01-02T03:05:05Z"}`
	var run FinalCheckRun
	if err := json.Unmarshal([]byte(recorded), &run); err != nil {
		t.Fatal(err)
	}
	if run.Counts == nil || run.Counts.Cancelled != 1 || run.Counts.Total != 10 {
		t.Fatalf("run %+v", run)
	}
	evidence := FinalCheckEvidence(run)
	for _, want := range []string{"10 checks, 9 passed, 0 failed, 1 cancelled", "https://dagger.cloud/checks/3.json"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("evidence lacks %q:\n%s", want, evidence)
		}
	}
}
