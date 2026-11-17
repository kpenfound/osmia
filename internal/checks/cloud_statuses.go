package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
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
	// CloudIncomplete is a completed run with no failure in which some check
	// was cancelled or has no result.
	CloudIncomplete CloudOutcome = "incomplete"
	// CloudFailed is a completed run in which a check failed.
	CloudFailed CloudOutcome = "failed"
)

// CloudCounts are the check counts of a Cloud run.
type CloudCounts struct {
	Total     int `json:"total"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// CloudStatusDocument is the JSON the "Run Dagger Checks" status points at.
type CloudStatusDocument struct {
	// Status is the run's state; "completed" is the only final one.
	Status string      `json:"status"`
	Checks CloudCounts `json:"checks"`
}

// Completed reports whether the run has finished.
func (d CloudStatusDocument) Completed() bool { return d.Status == "completed" }

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
// differ in Run.ID and its times. Counts is set for a completed run. Failed
// lists the failing checks of a failed result, sorted by name. Checks lists
// every check the statuses report for a completed run, sorted by name: all the
// contexts but `load` and the run's own.
type CloudResult struct {
	Outcome    CloudOutcome         `json:"outcome"`
	Repository string               `json:"repository"`
	Commit     string               `json:"commit"`
	Load       *pulls.CommitStatus  `json:"load,omitempty"`
	Run        *pulls.CommitStatus  `json:"run,omitempty"`
	Counts     *CloudCounts         `json:"counts,omitempty"`
	Failed     []CloudFailedCheck   `json:"failed,omitempty"`
	Checks     []CloudCheck         `json:"checks,omitempty"`
	StatusURL  string               `json:"status_url,omitempty"`
	Detail     string               `json:"detail,omitempty"`
	Document   *CloudStatusDocument `json:"-"`
}

// IsFailure reports whether the result is a failed one: a completed run in
// which a check failed, or a `load` that failed.
func (r CloudResult) IsFailure() bool {
	return r.Outcome == CloudFailed || r.Outcome == CloudLoadFailed
}

// Completed reports whether the run reached a final result: it passed,
// failed, or finished with checks cancelled or unaccounted for.
func (r CloudResult) Completed() bool {
	switch r.Outcome {
	case CloudPassed, CloudIncomplete, CloudFailed:
		return true
	}
	return false
}

// ErrCloudStatusUnavailable is a finished "Run Dagger Checks" status whose
// JSON was not read: the caller counts the run as incomplete, never as passed.
var ErrCloudStatusUnavailable = errors.New("the Dagger Cloud status JSON is unavailable")

// InterpretCloudStatuses turns one combined-status read into a result. doc is
// the parsed JSON of the "Run Dagger Checks" status, or nil when it has not
// been read. A "Run Dagger Checks" status that is not pending needs doc;
// without it the error wraps ErrCloudStatusUnavailable and the result holds
// the statuses but no outcome. The overall state comes only from doc, never
// from the per-check statuses.
func InterpretCloudStatuses(read pulls.CommitStatuses, doc *CloudStatusDocument) (CloudResult, error) {
	result := CloudResult{Repository: read.Repository, Commit: read.Commit}
	load, ok := read.Status(CloudLoadContext)
	if !ok {
		result.Outcome = CloudLoadMissing
		return result, nil
	}
	result.Load = &load
	if failing(load.State) {
		result.Outcome = CloudLoadFailed
		result.Failed = []CloudFailedCheck{failedCheck(load)}
		if run, ok := read.Status(CloudRunContext); ok {
			result.Run, result.StatusURL = &run, run.TargetURL
		}
		return result, nil
	}
	run, ok := read.Status(CloudRunContext)
	if ok {
		result.Run, result.StatusURL = &run, run.TargetURL
	}
	if !ok || run.State == "pending" {
		result.Outcome = CloudPending
		return result, nil
	}
	if doc == nil {
		return result, fmt.Errorf("%w for the %q status %d", ErrCloudStatusUnavailable, CloudRunContext, run.ID)
	}
	result.Document = doc
	if !doc.Completed() {
		result.Outcome = CloudPending
		return result, nil
	}
	counts := doc.Checks
	result.Counts = &counts
	result.Checks = cloudChecks(read)
	switch {
	case counts.Failed > 0:
		result.Outcome = CloudFailed
		for _, s := range read.Statuses {
			if s.Context != CloudLoadContext && s.Context != CloudRunContext && failing(s.State) {
				result.Failed = append(result.Failed, failedCheck(s))
			}
		}
		sort.Slice(result.Failed, func(i, j int) bool { return result.Failed[i].Name < result.Failed[j].Name })
	case counts.Cancelled == 0 && counts.Total > 0 && counts.Passed == counts.Total:
		result.Outcome = CloudPassed
	default:
		result.Outcome = CloudIncomplete
		switch {
		case counts.Total == 0:
			result.Detail = "the run reports no checks"
		case counts.Cancelled > 0:
			result.Detail = fmt.Sprintf("%d of %d checks were cancelled", counts.Cancelled, counts.Total)
		default:
			result.Detail = fmt.Sprintf("%d of %d checks have no result", counts.Total-counts.Passed-counts.Cancelled, counts.Total)
		}
	}
	return result, nil
}

// cloudChecks lists the checks the statuses report, sorted by name.
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

// statusDuration returns the duration a status description reports, as in
// "Failed in 2m33s.", or zero when it reports none.
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

// maxCloudStatusJSON bounds the "Run Dagger Checks" JSON that is read.
const maxCloudStatusJSON = 1 << 20

// CloudStatusFetcher reads the JSON a "Run Dagger Checks" status points at. It
// sends no credentials: the GitHub token never leaves the GitHub client.
type CloudStatusFetcher struct {
	// HTTP is the client used; nil means a client with a 30 second timeout.
	HTTP *http.Client
	// Allow decides which URLs may be read, including the targets of
	// redirects; nil allows only https://dagger.cloud.
	Allow func(*url.URL) bool
}

func (f CloudStatusFetcher) allowed(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	if f.Allow != nil {
		return f.Allow(u)
	}
	return u.Scheme == "https" && u.Hostname() == "dagger.cloud"
}

// Fetch reads and parses the JSON at target. An unreadable, oversized,
// malformed or unexpected document is an error.
func (f CloudStatusFetcher) Fetch(ctx context.Context, target string) (CloudStatusDocument, error) {
	u, err := url.Parse(target)
	if err != nil || !f.allowed(u) {
		return CloudStatusDocument{}, fmt.Errorf("%w: %q is not a Dagger Cloud status URL", ErrCloudStatusUnavailable, hostOnly(target))
	}
	client := http.Client{Timeout: 30 * time.Second}
	if f.HTTP != nil {
		client = *f.HTTP
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !f.allowed(req.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return CloudStatusDocument{}, fmt.Errorf("%w: %v", ErrCloudStatusUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return CloudStatusDocument{}, fmt.Errorf("%w: the request failed", ErrCloudStatusUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CloudStatusDocument{}, fmt.Errorf("%w: Dagger Cloud answered %d", ErrCloudStatusUnavailable, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudStatusJSON+1))
	if err != nil || len(data) > maxCloudStatusJSON {
		return CloudStatusDocument{}, fmt.Errorf("%w: the response is unreadable or too large", ErrCloudStatusUnavailable)
	}
	return ParseCloudStatusDocument(data)
}

// hostOnly keeps only the scheme and host of a URL for error text.
func hostOnly(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "an unparsable URL"
}

// ParseCloudStatusDocument parses {"dagger":{"status":...,"checks":{...}}},
// ignoring fields it does not know. A document without the dagger object, a
// status, or with a negative count is an error.
func ParseCloudStatusDocument(data []byte) (CloudStatusDocument, error) {
	var raw struct {
		Dagger *CloudStatusDocument `json:"dagger"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return CloudStatusDocument{}, fmt.Errorf("%w: malformed JSON", ErrCloudStatusUnavailable)
	}
	if raw.Dagger == nil || strings.TrimSpace(raw.Dagger.Status) == "" {
		return CloudStatusDocument{}, fmt.Errorf("%w: no dagger status in the JSON", ErrCloudStatusUnavailable)
	}
	c := raw.Dagger.Checks
	if c.Total < 0 || c.Passed < 0 || c.Failed < 0 || c.Cancelled < 0 {
		return CloudStatusDocument{}, fmt.Errorf("%w: negative check counts", ErrCloudStatusUnavailable)
	}
	return *raw.Dagger, nil
}

// ReadCloudChecks reads the combined statuses of commit in repository and
// interprets them, reading the "Run Dagger Checks" JSON when that status is
// not pending. An error means the read or the JSON failed; the caller counts
// the run as incomplete and never as passed.
func ReadCloudChecks(ctx context.Context, reader pulls.StatusReader, fetcher CloudStatusFetcher, repository, commit string) (CloudResult, error) {
	result, _, err := ReadCloudStatuses(ctx, reader, fetcher, repository, commit)
	return result, err
}

// ReadCloudStatuses is ReadCloudChecks that also returns the statuses it
// read, so a caller can look at the status of any one check in the same read.
// The statuses are empty when the read itself failed.
func ReadCloudStatuses(ctx context.Context, reader pulls.StatusReader, fetcher CloudStatusFetcher, repository, commit string) (CloudResult, pulls.CommitStatuses, error) {
	read, err := reader.CommitStatuses(ctx, repository, commit)
	if err != nil {
		return CloudResult{}, pulls.CommitStatuses{}, err
	}
	var doc *CloudStatusDocument
	if run, ok := read.Status(CloudRunContext); ok && run.State != "pending" {
		if load, ok := read.Status(CloudLoadContext); ok && !failing(load.State) {
			d, err := fetcher.Fetch(ctx, run.TargetURL)
			if err != nil {
				result, _ := InterpretCloudStatuses(read, nil)
				return result, read, err
			}
			doc = &d
		}
	}
	result, err := InterpretCloudStatuses(read, doc)
	return result, read, err
}
