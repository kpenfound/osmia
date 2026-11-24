package checks

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/pulls"
)

var cloudEpoch = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

func cloudStatus(id int64, name, state, description, target string) pulls.CommitStatus {
	at := cloudEpoch.Add(time.Duration(id) * time.Minute)
	return pulls.CommitStatus{ID: id, Context: name, State: state, Description: description, TargetURL: target, CreatedAt: at, UpdatedAt: at}
}

func cloudRead(statuses ...pulls.CommitStatus) pulls.CommitStatuses {
	return pulls.CommitStatuses{Repository: "acme/widgets", Commit: "abc123", Statuses: statuses}
}

const runTarget = "https://dagger.cloud/acme/checks/github.com/acme/widgets@abc123?check=Run+Dagger+Checks"

func TestInterpretCloudStatuses(t *testing.T) {
	t.Parallel()
	load := cloudStatus(1, "load", "success", "Loaded", "")
	run := func(state string) pulls.CommitStatus { return cloudStatus(5, CloudRunContext, state, "", runTarget) }
	check := func(id int64, state string) pulls.CommitStatus {
		return cloudStatus(id, fmt.Sprintf("check-%d", id), state, "", "")
	}
	tests := []struct {
		name     string
		read     pulls.CommitStatuses
		outcome  CloudOutcome
		counts   *CloudCounts
		detail   string
		failed   []string
		statuses int
	}{
		{name: "load missing", read: cloudRead(run("success"), check(7, "success")), outcome: CloudLoadMissing},
		{name: "load failed", read: cloudRead(cloudStatus(1, "load", "failure", "Failed. Run `dagger trace loadtrace`", "")), outcome: CloudLoadFailed, failed: []string{"load"}},
		{name: "load errored", read: cloudRead(cloudStatus(1, "load", "error", "", "")), outcome: CloudLoadFailed, failed: []string{"load"}},
		{name: "run absent", read: cloudRead(load), outcome: CloudPending},
		{name: "run absent with checks posted", read: cloudRead(load, check(7, "success")), outcome: CloudPending},
		{name: "run pending", read: cloudRead(load, run("pending"), check(7, "success")), outcome: CloudPending},
		{name: "run success, every check success", read: cloudRead(load, run("success"), check(7, "success"), check(8, "success"), check(9, "success")), outcome: CloudPassed, counts: &CloudCounts{Total: 3, Passed: 3}},
		{name: "run success, one check success", read: cloudRead(load, run("success"), check(7, "success")), outcome: CloudPassed, counts: &CloudCounts{Total: 1, Passed: 1}},
		{name: "run success, a check pending", read: cloudRead(load, run("success"), check(7, "success"), check(8, "pending"), check(9, "success")), outcome: CloudIncomplete, counts: &CloudCounts{Total: 3, Passed: 2}, detail: "1 of 3 checks have no result"},
		{name: "run success, every check pending", read: cloudRead(load, run("success"), check(7, "pending"), check(8, "pending")), outcome: CloudIncomplete, counts: &CloudCounts{Total: 2}, detail: "2 of 2 checks have no result"},
		{name: "run success, a check in an unknown state", read: cloudRead(load, run("success"), check(7, "success"), check(8, "cancelled")), outcome: CloudIncomplete, counts: &CloudCounts{Total: 2, Passed: 1}, detail: "1 of 2 checks have no result"},
		{name: "run success, zero checks", read: cloudRead(load, run("success")), outcome: CloudIncomplete, counts: &CloudCounts{}, detail: "the run reports no checks"},
		{name: "run success, load pending", read: cloudRead(cloudStatus(1, "load", "pending", "", ""), run("success"), check(7, "success")), outcome: CloudIncomplete, counts: &CloudCounts{Total: 1, Passed: 1}, detail: `the "load" status is pending`},
		{name: "run failure, every check success", read: cloudRead(load, run("failure"), check(7, "success"), check(8, "success")), outcome: CloudFailed, counts: &CloudCounts{Total: 2, Passed: 2}, detail: `the "Run Dagger Checks" status is failure but no check status failed`},
		{name: "run error, every check success", read: cloudRead(load, run("error"), check(7, "success")), outcome: CloudFailed, counts: &CloudCounts{Total: 1, Passed: 1}, detail: `the "Run Dagger Checks" status is error but no check status failed`},
		{name: "run failure, no checks", read: cloudRead(load, run("failure")), outcome: CloudFailed, counts: &CloudCounts{}, detail: `the "Run Dagger Checks" status is failure but no check status failed`},
		{name: "run failure, a check pending", read: cloudRead(load, run("failure"), check(7, "pending")), outcome: CloudFailed, counts: &CloudCounts{Total: 1}, detail: `the "Run Dagger Checks" status is failure but no check status failed`},
		{name: "run success, a check failed", read: cloudRead(load, run("success"), check(7, "success"), check(8, "failure")), outcome: CloudFailed, counts: &CloudCounts{Total: 2, Passed: 1, Failed: 1}, failed: []string{"check-8"}},
		{name: "run failure, checks failed and pending", read: cloudRead(load, run("failure"), check(7, "error"), check(8, "failure"), check(9, "pending")), outcome: CloudFailed, counts: &CloudCounts{Total: 3, Failed: 2}, failed: []string{"check-7", "check-8"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := InterpretCloudStatuses(tc.read)
			if got.Outcome != tc.outcome {
				t.Fatalf("outcome %q, want %q: %+v", got.Outcome, tc.outcome, got)
			}
			if (got.Counts == nil) != (tc.counts == nil) || (tc.counts != nil && *got.Counts != *tc.counts) {
				t.Fatalf("counts %+v, want %+v", got.Counts, tc.counts)
			}
			if got.Counts != nil && got.Counts.Cancelled != 0 {
				t.Fatalf("cancelled %d", got.Counts.Cancelled)
			}
			if got.Detail != tc.detail {
				t.Fatalf("detail %q, want %q", got.Detail, tc.detail)
			}
			var failed []string
			for _, f := range got.Failed {
				failed = append(failed, f.Name)
			}
			if !reflect.DeepEqual(failed, tc.failed) {
				t.Fatalf("failed checks %v, want %v", failed, tc.failed)
			}
			if got.Repository != "acme/widgets" || got.Commit != "abc123" {
				t.Fatalf("result %+v", got)
			}
			if got.IsFailure() != (tc.outcome == CloudFailed || tc.outcome == CloudLoadFailed) {
				t.Fatalf("IsFailure %v for %q", got.IsFailure(), got.Outcome)
			}
			if tc.outcome == CloudPassed && (got.Counts.Total != got.Counts.Passed || got.Counts.Failed != 0) {
				t.Fatalf("passed counts %+v", got.Counts)
			}
		})
	}
}

func TestCountsComeFromThePerCheckStatusesAlone(t *testing.T) {
	t.Parallel()
	read := cloudRead(
		cloudStatus(1, "load", "success", "", ""),
		cloudStatus(5, CloudRunContext, "success", "", runTarget),
		cloudStatus(7, "lint", "success", "", ""),
		cloudStatus(8, "test", "success", "", ""),
	)
	got := InterpretCloudStatuses(read)
	if got.Counts == nil || *got.Counts != (CloudCounts{Total: 2, Passed: 2}) || len(got.Checks) != 2 {
		t.Fatalf("counts %+v, checks %+v", got.Counts, got.Checks)
	}
}

func TestStatusURLIsTheRunTargetAndIsNotFetched(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]pulls.CommitStatuses{
		"passed":  cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "success", "", runTarget), cloudStatus(7, "lint", "success", "", "https://dagger.cloud/other")),
		"failed":  cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "failure", "", runTarget)),
		"pending": cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "pending", "", runTarget)),
		"load":    cloudRead(cloudStatus(1, "load", "failure", "", ""), cloudStatus(5, CloudRunContext, "pending", "", runTarget)),
	} {
		got, _, err := ReadCloudStatuses(context.Background(), cloudFake{statuses: read}, "acme/widgets", "abc123")
		if err != nil || got.StatusURL != runTarget {
			t.Errorf("%s: status url %q: %v", name, got.StatusURL, err)
		}
	}
}

func TestCloudFailureNamesFailedChecksWithTraceIDs(t *testing.T) {
	t.Parallel()
	read := cloudRead(
		cloudStatus(1, "load", "success", "Loaded", ""),
		cloudStatus(9, CloudRunContext, "failure", "1 failed", runTarget),
		cloudStatus(11, "test-split/test-cli-engine", "failure", "Failed in 2m33s. Run `dagger trace 0a1b2c3d4e5f` to see why", ""),
		cloudStatus(10, "lint", "error", "Errored", ""),
		cloudStatus(12, "build", "success", "Passed in 1m. Run `dagger trace ffff`", ""),
		cloudStatus(13, "docs", "pending", "", ""),
	)
	got := InterpretCloudStatuses(read)
	if got.Outcome != CloudFailed || len(got.Failed) != 2 || *got.Counts != (CloudCounts{Total: 4, Passed: 1, Failed: 2}) {
		t.Fatalf("result %+v", got)
	}
	if got.Failed[0].Name != "lint" || got.Failed[0].TraceID != "" || got.Failed[0].Status.ID != 10 {
		t.Fatalf("first failed check %+v", got.Failed[0])
	}
	second := got.Failed[1]
	if second.Name != "test-split/test-cli-engine" || second.TraceID != "0a1b2c3d4e5f" || second.Status.ID != 11 || !second.Status.UpdatedAt.Equal(cloudEpoch.Add(11*time.Minute)) {
		t.Fatalf("second failed check %+v", second)
	}
	if got.Checks[3].Name != "test-split/test-cli-engine" || got.Checks[3].Duration != 2*time.Minute+33*time.Second || got.Checks[3].TraceID != "0a1b2c3d4e5f" {
		t.Fatalf("checks %+v", got.Checks)
	}
}

func TestCloudLoadFailureIsAFailedResultNamingLoad(t *testing.T) {
	t.Parallel()
	got := InterpretCloudStatuses(cloudRead(cloudStatus(1, "load", "failure", "Failed. Run `dagger trace loadtrace`", "")))
	if !got.IsFailure() || got.Outcome != CloudLoadFailed || len(got.Failed) != 1 || got.Failed[0].Name != "load" || got.Failed[0].TraceID != "loadtrace" {
		t.Fatalf("result %+v", got)
	}
}

func TestARunSuccessAloneIsNeverAPass(t *testing.T) {
	t.Parallel()
	read := cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "success", "", runTarget))
	if got := InterpretCloudStatuses(read); got.Outcome == CloudPassed {
		t.Fatalf("result %+v", got)
	}
}

func TestResultsDistinguishPostingsOfTheRunStatus(t *testing.T) {
	t.Parallel()
	load := cloudStatus(1, "load", "success", "", "")
	first := cloudStatus(5, CloudRunContext, "failure", "", runTarget)
	second := cloudStatus(20, CloudRunContext, "success", "", runTarget)
	test := cloudStatus(6, "test/a", "success", "", "")
	a := InterpretCloudStatuses(cloudRead(load, first, test))
	b := InterpretCloudStatuses(cloudRead(load, second, test))
	if a.Run == nil || b.Run == nil || a.Run.ID != 5 || b.Run.ID != 20 || !b.Run.CreatedAt.After(a.Run.UpdatedAt) {
		t.Fatalf("runs %+v / %+v", a.Run, b.Run)
	}
	if a.Load == nil || a.Load.ID != 1 || a.StatusURL != runTarget {
		t.Fatalf("result %+v", a)
	}
	p := InterpretCloudStatuses(cloudRead(load, cloudStatus(3, CloudRunContext, "pending", "", "")))
	if p.Outcome != CloudPending || p.Run == nil || p.Run.ID != 3 {
		t.Fatalf("pending %+v", p)
	}
}

type cloudFake struct {
	statuses pulls.CommitStatuses
	err      error
}

func (f cloudFake) CommitStatuses(context.Context, string, string) (pulls.CommitStatuses, error) {
	return f.statuses, f.err
}

func TestReadCloudStatusesReadsOnlyTheStatuses(t *testing.T) {
	t.Parallel()
	reader := cloudFake{statuses: cloudRead(
		cloudStatus(1, "load", "success", "", ""),
		cloudStatus(5, CloudRunContext, "failure", "", "http://127.0.0.1:1/never-fetched"),
		cloudStatus(7, "test/a", "failure", "Failed. Run `dagger trace t1`", "http://127.0.0.1:1/never-fetched"),
		cloudStatus(8, "test/b", "success", "", ""),
	)}
	got, read, err := ReadCloudStatuses(context.Background(), reader, "acme/widgets", "abc123")
	if err != nil || got.Outcome != CloudFailed || got.Counts == nil || *got.Counts != (CloudCounts{Total: 2, Passed: 1, Failed: 1}) || len(got.Failed) != 1 || got.Failed[0].TraceID != "t1" || len(read.Statuses) != 4 {
		t.Fatalf("result %+v: %v", got, err)
	}
}

func TestReadCloudStatusesPassesOnGitHubFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("github is down")
	if _, _, err := ReadCloudStatuses(context.Background(), cloudFake{err: boom}, "acme/widgets", "abc123"); !errors.Is(err, boom) {
		t.Fatalf("error %v", err)
	}
}
