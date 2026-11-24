package checks

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/kpenfound/osmia/internal/pulls"
)

// The commit status contexts Dagger Cloud posts on a pull request's commit.
const (
	// CloudLoadContext is the status of loading the module.
	CloudLoadContext = "load"
	// CloudRunContext is the overall status of the run. It is posted several
	// times for one commit: pending, then success or failure.
	CloudRunContext = "Run Dagger Checks"
)

// CloudOutcome is what one read of a commit's Dagger Cloud statuses shows.
type CloudOutcome string

const (
	// CloudLoadMissing is a commit with no `load` status.
	CloudLoadMissing CloudOutcome = "load_missing"
	// CloudLoadFailed is a commit whose `load` status failed. It is a failed
	// result (see IsFailure) that names `load` as its failed check.
	CloudLoadFailed CloudOutcome = "load_failed"
	// CloudPending is a commit whose `load` ran but whose run has not
	// completed.
	CloudPending CloudOutcome = "pending"
	// CloudPassed is a completed run in which every check passed.
	CloudPassed CloudOutcome = "passed"
	// CloudIncomplete is a completed run with no failure in which the run
	// reports no checks or some check has no result.
	CloudIncomplete CloudOutcome = "incomplete"
	// CloudFailed is a completed run in which a check failed.
	CloudFailed CloudOutcome = "failed"
)

// CloudCounts are the check counts of a Cloud run, taken from the per-check
// statuses: every context but `load` and the run's own. Cancelled is always
// zero, since a commit status has no cancelled state.
type CloudCounts struct {
	Total     int `json:"total"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// CloudFailedCheck is one check that failed, from its own commit status.
// TraceID is the `dagger trace` id its description names, when it names one.
type CloudFailedCheck struct {
	Name    string             `json:"name"`
	TraceID string             `json:"trace_id,omitempty"`
	Status  pulls.CommitStatus `json:"status"`
}

// CloudCheck is one check the commit's statuses report, whatever its state.
// TraceID is the `dagger trace` id its description names, when it names one,
// and Duration the time the description says it took, zero when it names none.
type CloudCheck struct {
	Name      string        `json:"name"`
	State     string        `json:"state"`
	StatusID  int64         `json:"status_id"`
	TraceID   string        `json:"trace_id,omitempty"`
	TargetURL string        `json:"target_url,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
}

// CloudResult is the interpretation of one read of a commit's statuses.
//
// Load is the `load` status and Run the "Run Dagger Checks" status the result
// was read from; two reads of one commit that saw different postings of Run
// differ in Run.ID and its times. Counts is set for a completed run. StatusURL
// is the link to the Cloud page the run status names; it is never fetched. Failed
// lists the failing checks of a failed result, sorted by name. Checks lists
// every check the statuses report for a completed run, sorted by name: all the
// contexts but `load` and the run's own.
type CloudResult struct {
	Outcome    CloudOutcome        `json:"outcome"`
	Repository string              `json:"repository"`
	Commit     string              `json:"commit"`
	Load       *pulls.CommitStatus `json:"load,omitempty"`
	Run        *pulls.CommitStatus `json:"run,omitempty"`
	Counts     *CloudCounts        `json:"counts,omitempty"`
	Failed     []CloudFailedCheck  `json:"failed,omitempty"`
	Checks     []CloudCheck        `json:"checks,omitempty"`
	StatusURL  string              `json:"status_url,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}

// IsFailure reports whether the result is a failed one: a completed run in
// which a check failed, or a `load` that failed.
func (r CloudResult) IsFailure() bool {
	return r.Outcome == CloudFailed || r.Outcome == CloudLoadFailed
}

// InterpretCloudStatuses turns one combined-status read into a result, from the
// statuses alone.
//
// A missing `load` is CloudLoadMissing and a failed one CloudLoadFailed. A
// "Run Dagger Checks" status that is absent or not yet success, failure or
// error is CloudPending. Once the run has completed, the counts come from the
// per-check statuses: total is all of them, passed those in success and failed
// those in failure or error. The result is failed when the run or any check
// failed, passed only when `load` and the run succeeded and there is at least
// one check, every one of them a success, and incomplete otherwise.
func InterpretCloudStatuses(read pulls.CommitStatuses) CloudResult {
	result := CloudResult{Repository: read.Repository, Commit: read.Commit}
	load, ok := read.Status(CloudLoadContext)
	if !ok {
		result.Outcome = CloudLoadMissing
		return result
	}
	result.Load = &load
	run, hasRun := read.Status(CloudRunContext)
	if hasRun {
		result.Run, result.StatusURL = &run, run.TargetURL
	}
	if failing(load.State) {
		result.Outcome = CloudLoadFailed
		result.Failed = []CloudFailedCheck{failedCheck(load)}
		return result
	}
	if !hasRun || !runCompleted(run.State) {
		result.Outcome = CloudPending
		return result
	}
	result.Checks = cloudChecks(read)
	counts := CloudCounts{Total: len(result.Checks)}
	for _, c := range result.Checks {
		switch {
		case c.State == "success":
			counts.Passed++
		case failing(c.State):
			counts.Failed++
		}
	}
	result.Counts = &counts
	if counts.Failed > 0 {
		for _, s := range read.Statuses {
			if s.Context != CloudLoadContext && s.Context != CloudRunContext && failing(s.State) {
				result.Failed = append(result.Failed, failedCheck(s))
			}
		}
		sort.Slice(result.Failed, func(i, j int) bool { return result.Failed[i].Name < result.Failed[j].Name })
	}
	switch {
	case failing(run.State) || counts.Failed > 0:
		result.Outcome = CloudFailed
		if counts.Failed == 0 {
			result.Detail = fmt.Sprintf("the %q status is %s but no check status failed", CloudRunContext, run.State)
		}
	case load.State == "success" && counts.Total > 0 && counts.Passed == counts.Total:
		result.Outcome = CloudPassed
	default:
		result.Outcome = CloudIncomplete
		switch {
		case load.State != "success":
			result.Detail = fmt.Sprintf("the %q status is %s", CloudLoadContext, load.State)
		case counts.Total == 0:
			result.Detail = "the run reports no checks"
		default:
			result.Detail = fmt.Sprintf("%d of %d checks have no result", counts.Total-counts.Passed-counts.Failed, counts.Total)
		}
	}
	return result
}

func runCompleted(state string) bool { return state == "success" || failing(state) }

func cloudChecks(read pulls.CommitStatuses) []CloudCheck {
	var checks []CloudCheck
	for _, s := range read.Statuses {
		if s.Context == CloudLoadContext || s.Context == CloudRunContext {
			continue
		}
		check := CloudCheck{Name: s.Context, State: s.State, StatusID: s.ID, TargetURL: s.TargetURL, Duration: statusDuration(s.Description)}
		if m := traceDescription.FindStringSubmatch(s.Description); m != nil {
			check.TraceID = m[1]
		}
		checks = append(checks, check)
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	return checks
}

var durationDescription = regexp.MustCompile(`\bin ((?:[0-9]+(?:\.[0-9]+)?(?:ms|h|m|s))+)`)

func statusDuration(description string) time.Duration {
	m := durationDescription.FindStringSubmatch(description)
	if m == nil {
		return 0
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		return 0
	}
	return d
}

func failing(state string) bool { return state == "failure" || state == "error" }

var traceDescription = regexp.MustCompile("dagger trace ([A-Za-z0-9_-]+)")

func failedCheck(s pulls.CommitStatus) CloudFailedCheck {
	check := CloudFailedCheck{Name: s.Context, Status: s}
	if m := traceDescription.FindStringSubmatch(s.Description); m != nil {
		check.TraceID = m[1]
	}
	return check
}

// ReadCloudStatuses reads the combined statuses of commit in repository and
// interprets them. It also returns the statuses it read, so a caller can look
// at the status of any one check in the same read. The statuses are empty when
// the read itself failed.
func ReadCloudStatuses(ctx context.Context, reader pulls.StatusReader, repository, commit string) (CloudResult, pulls.CommitStatuses, error) {
	read, err := reader.CommitStatuses(ctx, repository, commit)
	if err != nil {
		return CloudResult{}, pulls.CommitStatuses{}, err
	}
	return InterpretCloudStatuses(read), read, nil
}
