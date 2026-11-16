package checks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func cloudDoc(status string, total, passed, failed, cancelled int) *CloudStatusDocument {
	return &CloudStatusDocument{Status: status, Checks: CloudCounts{Total: total, Passed: passed, Failed: failed, Cancelled: cancelled}}
}

func TestInterpretCloudStatuses(t *testing.T) {
	t.Parallel()
	load := cloudStatus(1, "load", "success", "Loaded", "")
	run := cloudStatus(5, CloudRunContext, "success", "", "https://dagger.cloud/checks/x.json")
	pending := cloudStatus(4, CloudRunContext, "pending", "", "https://dagger.cloud/checks/x.json")
	failedLoad := cloudStatus(1, "load", "failure", "Failed. Run `dagger trace loadtrace`", "")
	tests := []struct {
		name    string
		read    pulls.CommitStatuses
		doc     *CloudStatusDocument
		outcome CloudOutcome
		counts  *CloudCounts
	}{
		{"no load", cloudRead(run, cloudStatus(7, "test/a", "success", "", "")), cloudDoc("completed", 2, 2, 0, 0), CloudLoadMissing, nil},
		{"failed load", cloudRead(failedLoad), nil, CloudLoadFailed, nil},
		{"errored load", cloudRead(cloudStatus(1, "load", "error", "", "")), nil, CloudLoadFailed, nil},
		{"load only", cloudRead(load), nil, CloudPending, nil},
		{"pending run", cloudRead(load, pending), nil, CloudPending, nil},
		{"json not completed", cloudRead(load, run), cloudDoc("running", 3, 1, 0, 0), CloudPending, nil},
		{"passed", cloudRead(load, run), cloudDoc("completed", 92, 92, 0, 0), CloudPassed, &CloudCounts{Total: 92, Passed: 92}},
		{"cancelled", cloudRead(load, run), cloudDoc("completed", 2, 1, 0, 1), CloudIncomplete, &CloudCounts{Total: 2, Passed: 1, Cancelled: 1}},
		{"unaccounted", cloudRead(load, run), cloudDoc("completed", 5, 3, 0, 0), CloudIncomplete, &CloudCounts{Total: 5, Passed: 3}},
		{"over-accounted", cloudRead(load, run), cloudDoc("completed", 2, 3, 0, 0), CloudIncomplete, &CloudCounts{Total: 2, Passed: 3}},
		{"no checks", cloudRead(load, run), cloudDoc("completed", 0, 0, 0, 0), CloudIncomplete, &CloudCounts{}},
		{"failed", cloudRead(load, run), cloudDoc("completed", 92, 91, 1, 0), CloudFailed, &CloudCounts{Total: 92, Passed: 91, Failed: 1}},
		{"failed with cancelled", cloudRead(load, run), cloudDoc("completed", 4, 1, 1, 2), CloudFailed, &CloudCounts{Total: 4, Passed: 1, Failed: 1, Cancelled: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := InterpretCloudStatuses(tc.read, tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != tc.outcome {
				t.Fatalf("outcome %q, want %q: %+v", got.Outcome, tc.outcome, got)
			}
			if (got.Counts == nil) != (tc.counts == nil) || (tc.counts != nil && *got.Counts != *tc.counts) {
				t.Fatalf("counts %+v, want %+v", got.Counts, tc.counts)
			}
			if got.Repository != "acme/widgets" || got.Commit != "abc123" {
				t.Fatalf("result %+v", got)
			}
			if got.IsFailure() != (tc.outcome == CloudFailed || tc.outcome == CloudLoadFailed) {
				t.Fatalf("IsFailure %v for %q", got.IsFailure(), got.Outcome)
			}
			if got.Completed() != (tc.outcome == CloudPassed || tc.outcome == CloudIncomplete || tc.outcome == CloudFailed) {
				t.Fatalf("Completed %v for %q", got.Completed(), got.Outcome)
			}
		})
	}
}

func TestCloudFailureNamesFailedChecksWithTraceIDs(t *testing.T) {
	t.Parallel()
	read := cloudRead(
		cloudStatus(1, "load", "success", "Loaded", ""),
		cloudStatus(9, CloudRunContext, "failure", "1 failed", "https://dagger.cloud/checks/x.json"),
		cloudStatus(11, "test-split/test-cli-engine", "failure", "Failed in 2m33s. Run `dagger trace 0a1b2c3d4e5f` to see why", ""),
		cloudStatus(10, "lint", "error", "Errored", ""),
		cloudStatus(12, "build", "success", "Passed in 1m. Run `dagger trace ffff`", ""),
		cloudStatus(13, "docs", "pending", "", ""),
	)
	got, err := InterpretCloudStatuses(read, cloudDoc("completed", 5, 3, 2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != CloudFailed || len(got.Failed) != 2 {
		t.Fatalf("result %+v", got)
	}
	if got.Failed[0].Name != "lint" || got.Failed[0].TraceID != "" || got.Failed[0].Status.ID != 10 {
		t.Fatalf("first failed check %+v", got.Failed[0])
	}
	second := got.Failed[1]
	if second.Name != "test-split/test-cli-engine" || second.TraceID != "0a1b2c3d4e5f" || second.Status.ID != 11 || !second.Status.UpdatedAt.Equal(cloudEpoch.Add(11*time.Minute)) {
		t.Fatalf("second failed check %+v", second)
	}
}

func TestCloudLoadFailureIsAFailedResultNamingLoad(t *testing.T) {
	t.Parallel()
	got, err := InterpretCloudStatuses(cloudRead(cloudStatus(1, "load", "failure", "Failed. Run `dagger trace loadtrace`", "")), nil)
	if err != nil || !got.IsFailure() || got.Outcome != CloudLoadFailed || len(got.Failed) != 1 || got.Failed[0].Name != "load" || got.Failed[0].TraceID != "loadtrace" {
		t.Fatalf("result %+v: %v", got, err)
	}
}

func TestOnlyTheRunStatusDecidesTheOverallState(t *testing.T) {
	t.Parallel()
	read := cloudRead(
		cloudStatus(1, "load", "success", "", ""),
		cloudStatus(5, CloudRunContext, "success", "", "https://dagger.cloud/checks/x.json"),
		cloudStatus(6, "test/a", "failure", "Failed. Run `dagger trace t1`", ""),
	)
	got, err := InterpretCloudStatuses(read, cloudDoc("completed", 1, 1, 0, 0))
	if err != nil || got.Outcome != CloudPassed || len(got.Failed) != 0 {
		t.Fatalf("a failing per-check status changed the overall result: %+v: %v", got, err)
	}
}

func TestResultsDistinguishPostingsOfTheRunStatus(t *testing.T) {
	t.Parallel()
	load := cloudStatus(1, "load", "success", "", "")
	first := cloudStatus(5, CloudRunContext, "failure", "", "https://dagger.cloud/checks/x.json")
	second := cloudStatus(20, CloudRunContext, "success", "", "https://dagger.cloud/checks/x.json")
	a, _ := InterpretCloudStatuses(cloudRead(load, first), cloudDoc("completed", 2, 1, 1, 0))
	b, _ := InterpretCloudStatuses(cloudRead(load, second), cloudDoc("completed", 2, 2, 0, 0))
	if a.Run == nil || b.Run == nil || a.Run.ID != 5 || b.Run.ID != 20 || !b.Run.CreatedAt.After(a.Run.UpdatedAt) {
		t.Fatalf("runs %+v / %+v", a.Run, b.Run)
	}
	if a.Load == nil || a.Load.ID != 1 || a.StatusURL != "https://dagger.cloud/checks/x.json" {
		t.Fatalf("result %+v", a)
	}
	// A pending posting is identified too.
	p, _ := InterpretCloudStatuses(cloudRead(load, cloudStatus(3, CloudRunContext, "pending", "", "")), nil)
	if p.Outcome != CloudPending || p.Run == nil || p.Run.ID != 3 {
		t.Fatalf("pending %+v", p)
	}
}

func TestFinishedRunStatusWithoutItsJSONIsAnError(t *testing.T) {
	t.Parallel()
	read := cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "success", "", "https://dagger.cloud/x.json"))
	got, err := InterpretCloudStatuses(read, nil)
	if !errors.Is(err, ErrCloudStatusUnavailable) || got.Outcome == CloudPassed || got.Run == nil {
		t.Fatalf("result %+v: %v", got, err)
	}
}

// cloudFake stands in for dagger.cloud and the GitHub status API: remote
// systems whose real behaviour these fakes do not reproduce.
type cloudFake struct {
	statuses pulls.CommitStatuses
	err      error
}

func (f cloudFake) CommitStatuses(context.Context, string, string) (pulls.CommitStatuses, error) {
	return f.statuses, f.err
}

func TestReadCloudChecksFetchesTheJSONWithoutCredentials(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("credentials sent to the status host: %v", r.Header)
		}
		fmt.Fprint(w, `{"dagger":{"status":"completed","extra":{"a":1},"checks":{"total":92,"passed":91,"failed":1,"cancelled":0,"skipped":4}},"other":[1]}`)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	fetcher := CloudStatusFetcher{HTTP: server.Client(), Allow: func(u *url.URL) bool { return u.Host == host }}
	reader := cloudFake{statuses: cloudRead(
		cloudStatus(1, "load", "success", "", ""),
		cloudStatus(5, CloudRunContext, "failure", "", server.URL+"/checks/x.json"),
		cloudStatus(7, "test/a", "failure", "Failed. Run `dagger trace t1`", ""),
	)}
	got, err := ReadCloudChecks(context.Background(), reader, fetcher, "acme/widgets", "abc123")
	if err != nil || got.Outcome != CloudFailed || got.Counts == nil || got.Counts.Failed != 1 || got.Counts.Total != 92 || len(got.Failed) != 1 || got.Failed[0].TraceID != "t1" {
		t.Fatalf("result %+v: %v", got, err)
	}
}

func TestReadCloudChecksDoesNotFetchWhileThePostingIsPending(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the pending run's JSON was fetched") }))
	defer server.Close()
	fetcher := CloudStatusFetcher{HTTP: server.Client(), Allow: func(*url.URL) bool { return true }}
	reader := cloudFake{statuses: cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "pending", "", "http://unused/x.json"))}
	got, err := ReadCloudChecks(context.Background(), reader, fetcher, "acme/widgets", "abc123")
	if err != nil || got.Outcome != CloudPending {
		t.Fatalf("result %+v: %v", got, err)
	}
}

func TestReadCloudChecksReportsUnreadableJSONAsAnError(t *testing.T) {
	t.Parallel()
	bodies := map[string]func(http.ResponseWriter){
		"/malformed": func(w http.ResponseWriter) { fmt.Fprint(w, `{"dagger":`) },
		"/empty":     func(w http.ResponseWriter) { fmt.Fprint(w, `{}`) },
		"/negative":  func(w http.ResponseWriter) { fmt.Fprint(w, `{"dagger":{"status":"completed","checks":{"total":1,"passed":-1}}}`) },
		"/missing":   func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
		"/huge": func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"dagger":{"status":"completed","checks":{"total":1,"passed":1}},"pad":"`+strings.Repeat("x", maxCloudStatusJSON)+`"}`)
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := bodies[r.URL.Path]; ok {
			body(w)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	fetcher := CloudStatusFetcher{HTTP: server.Client(), Allow: func(u *url.URL) bool { return u.Host == host }}
	for path := range bodies {
		reader := cloudFake{statuses: cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "success", "", server.URL+path))}
		got, err := ReadCloudChecks(context.Background(), reader, fetcher, "acme/widgets", "abc123")
		if !errors.Is(err, ErrCloudStatusUnavailable) || got.Outcome == CloudPassed || got.Run == nil || got.Run.ID != 5 {
			t.Errorf("%s: result %+v: %v", path, got, err)
		}
	}
	// A server that is not there at all.
	dead := cloudFake{statuses: cloudRead(cloudStatus(1, "load", "success", "", ""), cloudStatus(5, CloudRunContext, "success", "", "http://127.0.0.1:1/x.json"))}
	open := CloudStatusFetcher{Allow: func(*url.URL) bool { return true }}
	if got, err := ReadCloudChecks(context.Background(), dead, open, "acme/widgets", "abc123"); !errors.Is(err, ErrCloudStatusUnavailable) || got.Outcome == CloudPassed {
		t.Errorf("unreachable: result %+v: %v", got, err)
	}
}

func TestCloudStatusFetcherReadsOnlyDaggerCloud(t *testing.T) {
	t.Parallel()
	var f CloudStatusFetcher
	for _, target := range []string{"http://dagger.cloud/x.json", "https://evil.example/x.json", "https://user:pw@dagger.cloud/x.json", "file:///etc/passwd", "", "https://dagger.cloud.evil.example/x.json"} {
		if _, err := f.Fetch(context.Background(), target); !errors.Is(err, ErrCloudStatusUnavailable) {
			t.Errorf("%q: %v", target, err)
		}
	}
	if err := func() error { _, err := f.Fetch(context.Background(), "https://user:pw@dagger.cloud/x"); return err }(); err != nil && strings.Contains(err.Error(), "pw") {
		t.Errorf("credentials in error: %v", err)
	}
}

func TestCloudStatusFetcherRefusesRedirectsOffHost(t *testing.T) {
	t.Parallel()
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a redirect left the allowed host")
	}))
	defer elsewhere.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	f := CloudStatusFetcher{HTTP: server.Client(), Allow: func(u *url.URL) bool { return u.Host == host }}
	if _, err := f.Fetch(context.Background(), server.URL+"/x.json"); !errors.Is(err, ErrCloudStatusUnavailable) {
		t.Fatalf("redirect: %v", err)
	}
}

func TestReadCloudChecksPassesOnGitHubFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("github is down")
	if _, err := ReadCloudChecks(context.Background(), cloudFake{err: boom}, CloudStatusFetcher{}, "acme/widgets", "abc123"); !errors.Is(err, boom) {
		t.Fatalf("error %v", err)
	}
}
