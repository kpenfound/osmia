package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/ghlog"
)

const filerToken = "ghp_sentinelreportertoken0123456789"

// fakeGitHub is a GitHub REST server holding issues and comments for one
// repository, paged by per_page and page.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []string
	auth     []string
	bodies   []string
	issues   []map[string]any
	comments map[int][]map[string]any
	// status, when set, answers every request with it and a GitHub-style error.
	status int
	// message is the error message sent with status.
	message string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, string(body))
	if f.status != 0 {
		w.WriteHeader(f.status)
		fmt.Fprintf(w, `{"message":%q}`, f.message)
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/repos/o/r/issues":
		var in struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		json.Unmarshal(body, &in)
		labels := []map[string]any{}
		for _, l := range in.Labels {
			labels = append(labels, map[string]any{"name": l})
		}
		issue := map[string]any{"number": len(f.issues) + 1, "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", len(f.issues)+1), "state": "open", "title": in.Title, "body": in.Body, "labels": labels, "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:04:05Z"}
		f.issues = append(f.issues, issue)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(issue)
	case r.Method == http.MethodGet && path == "/repos/o/r/issues":
		f.page(w, r, f.issues)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/o/r/issues/") && strings.HasSuffix(path, "/comments"):
		n, _ := strconv.Atoi(strings.Split(path, "/")[5])
		var all []map[string]any
		all = append(all, f.comments[n]...)
		f.page(w, r, all)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
		n, _ := strconv.Atoi(strings.Split(path, "/")[5])
		var in struct {
			Body string `json:"body"`
		}
		json.Unmarshal(body, &in)
		c := map[string]any{"id": 100 + len(f.comments[n]), "html_url": "https://github.com/o/r/issues/1#c", "body": in.Body, "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:04:05Z"}
		if f.comments == nil {
			f.comments = map[int][]map[string]any{}
		}
		f.comments[n] = append(f.comments[n], c)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(c)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/o/r/issues/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(path, "/repos/o/r/issues/"))
		if n < 1 || n > len(f.issues) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		json.NewEncoder(w).Encode(f.issues[n-1])
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// page answers one page of items and, like GitHub, a Link header when more follow.
func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per < 1 {
		per = 30
	}
	if page < 1 {
		page = 1
	}
	var matching []map[string]any
	for _, it := range items {
		if want := r.URL.Query().Get("labels"); want != "" && !hasLabels(it, strings.Split(want, ",")) {
			continue
		}
		matching = append(matching, it)
	}
	lo := (page - 1) * per
	hi := lo + per
	if lo > len(matching) {
		lo = len(matching)
	}
	if hi > len(matching) {
		hi = len(matching)
	}
	if hi < len(matching) {
		w.Header().Set("Link", fmt.Sprintf(`<http://elsewhere.invalid/next?page=%d>; rel="next"`, page+1))
	}
	if matching == nil {
		matching = []map[string]any{}
	}
	json.NewEncoder(w).Encode(matching[lo:hi])
}

func hasLabels(issue map[string]any, want []string) bool {
	have := map[string]bool{}
	labels, _ := issue["labels"].([]map[string]any)
	for _, l := range labels {
		have[l["name"].(string)] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

func (f *fakeGitHub) seedIssue(number int, state string, labels []string, pull bool) {
	ls := []map[string]any{}
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	issue := map[string]any{"number": number, "html_url": "https://github.com/o/r/issues/" + strconv.Itoa(number), "state": state, "title": "t" + strconv.Itoa(number), "body": "b", "labels": ls, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
	if pull {
		issue["pull_request"] = map[string]any{"url": "https://api.github.com/repos/o/r/pulls/" + strconv.Itoa(number)}
	}
	f.issues = append(f.issues, issue)
}

type activity struct {
	Level, Msg, Activity, Access, Action, Repository, Method, Path, Error, Outcome string
	Status                                                                         int
	NoResponse                                                                     bool `json:"no_response"`
	ErrorReporter                                                                  bool `json:"error_reporter"`
}

func records(t *testing.T, buf *bytes.Buffer) []activity {
	t.Helper()
	var out []activity
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r activity
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		out = append(out, r)
	}
	return out
}

func newFiler(t *testing.T) (Filer, *fakeGitHub, context.Context, *bytes.Buffer) {
	t.Helper()
	fake := &fakeGitHub{}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	return Filer{BaseURL: server.URL, Token: filerToken, HTTP: server.Client(), PerPage: 2}, fake, ctx, &buf
}

func TestFilerCreateSendsTitleBodyAndLabelsAndReturnsTheNumber(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	got, err := f.Create(ctx, "o/r", NewIssue{Title: "Title", Body: "Body", Labels: []string{"factory-error", "automated"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 1 || got.State != StateOpen || got.URL != "https://github.com/o/r/issues/1" {
		t.Fatalf("created %+v", got)
	}
	if fake.requests[0] != "POST /repos/o/r/issues" {
		t.Fatalf("requests %v", fake.requests)
	}
	var sent struct {
		Title, Body string
		Labels      []string
	}
	if err := json.Unmarshal([]byte(fake.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Title != "Title" || sent.Body != "Body" || strings.Join(sent.Labels, ",") != "factory-error,automated" {
		t.Fatalf("sent %+v", sent)
	}
	if fake.auth[0] != "Bearer "+filerToken {
		t.Fatalf("auth %q", fake.auth[0])
	}
}

func TestFilerCommentPostsToTheIssuesCommentsEndpoint(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	fake.seedIssue(7, "open", nil, false)
	c, err := f.Comment(ctx, "o/r", 7, "still occurring")
	if err != nil {
		t.Fatal(err)
	}
	if fake.requests[0] != "POST /repos/o/r/issues/7/comments" || c.Body != "still occurring" || c.ID == 0 {
		t.Fatalf("comment %+v requests %v", c, fake.requests)
	}
	if !strings.Contains(fake.bodies[0], `"body":"still occurring"`) {
		t.Fatalf("body %s", fake.bodies[0])
	}
}

func TestFilerStateReportsOpenOrClosed(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	fake.seedIssue(1, "open", nil, false)
	fake.seedIssue(2, "closed", nil, false)
	fake.seedIssue(3, "open", nil, true)
	for number, want := range map[int]string{1: StateOpen, 2: StateClosed} {
		got, err := f.State(ctx, "o/r", number)
		if err != nil || got != want {
			t.Fatalf("issue %d: %q, %v", number, got, err)
		}
	}
	if _, err := f.State(ctx, "o/r", 3); err == nil || !strings.Contains(err.Error(), "pull request") {
		t.Fatalf("a pull request number must not read as an issue: %v", err)
	}
	if _, err := f.State(ctx, "o/r", 99); !IsRejected(err) {
		t.Fatalf("a missing issue is a 404 rejection: %v", err)
	}
}

func TestFilerListIssuesReadsEveryPageAndLeavesOutPullRequests(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	// No issue carries the reporter's labels; one has other labels, one has none.
	fake.seedIssue(1, "open", nil, false)
	fake.seedIssue(2, "closed", []string{"bug"}, false)
	fake.seedIssue(3, "open", []string{"x"}, true)
	fake.seedIssue(4, "open", []string{"factory-error"}, false)
	fake.seedIssue(5, "closed", nil, false)
	fake.seedIssue(6, "open", []string{"factory-error", "automated"}, false)

	got, err := f.ListIssues(ctx, "o/r", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, i := range got {
		numbers = append(numbers, i.Number)
	}
	if fmt.Sprint(numbers) != "[1 2 4 5 6]" {
		t.Fatalf("numbers %v", numbers)
	}
	if got[1].State != StateClosed || strings.Join(got[1].Labels, ",") != "bug" || got[0].Body != "b" {
		t.Fatalf("issues %+v", got)
	}
	if len(fake.requests) < 3 {
		t.Fatalf("a result across pages needs several requests: %v", fake.requests)
	}
	for _, r := range fake.requests {
		u, _ := url.Parse(strings.TrimPrefix(r, "GET "))
		q := u.Query()
		if u.Path != "/repos/o/r/issues" || q.Get("state") != "all" || q.Get("since") != "2026-01-01T00:00:00Z" || q.Has("labels") || q.Get("per_page") != "2" {
			t.Fatalf("request %s", r)
		}
	}
	for _, a := range fake.auth {
		if strings.Contains(strings.Join(fake.requests, " "), "elsewhere.invalid") || a != "Bearer "+filerToken {
			t.Fatalf("the Link header must not steer requests: %v", fake.requests)
		}
	}
}

func TestFilerListIssuesFiltersByLabelsWhenGiven(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	fake.seedIssue(1, "open", nil, false)
	fake.seedIssue(2, "open", []string{"factory-error", "automated"}, false)
	fake.seedIssue(3, "closed", []string{"factory-error", "automated"}, false)
	fake.seedIssue(4, "open", []string{"factory-error", "automated"}, false)
	got, err := f.ListIssues(ctx, "o/r", time.Time{}, []string{"factory-error", "automated"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Number != 2 || got[2].Number != 4 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(fake.requests[0], "labels=factory-error%2Cautomated") || strings.Contains(fake.requests[0], "since=") {
		t.Fatalf("request %s", fake.requests[0])
	}
}

func TestFilerListIssuesStopsAfterAnExactlyFullLastPageWithoutALinkHeader(t *testing.T) {
	// A server that sends no Link header still pages by per_page and page.
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		switch r.URL.Query().Get("page") {
		case "1":
			io.WriteString(w, `[{"number":1,"state":"open"},{"number":2,"state":"open"}]`)
		case "2":
			io.WriteString(w, `[{"number":3,"state":"closed"},{"number":4,"state":"open"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer server.Close()
	f := Filer{BaseURL: server.URL, HTTP: server.Client(), PerPage: 2}
	got, err := f.ListIssues(context.Background(), "o/r", time.Time{}, nil)
	if err != nil || len(got) != 4 || strings.Join(pages, ",") != "1,2,3" {
		t.Fatalf("got %d issues, pages %v, %v", len(got), pages, err)
	}
}

func TestFilerListCommentsReadsEveryPageSince(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	fake.seedIssue(1, "open", nil, false)
	fake.comments = map[int][]map[string]any{1: {}}
	for i := 0; i < 5; i++ {
		fake.comments[1] = append(fake.comments[1], map[string]any{"id": i + 1, "body": fmt.Sprintf("c%d", i), "created_at": "2026-01-02T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"})
	}
	got, err := f.ListComments(ctx, "o/r", 1, time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[4].Body != "c4" || got[0].CreatedAt.IsZero() {
		t.Fatalf("got %+v", got)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("requests %v", fake.requests)
	}
	u, _ := url.Parse(strings.TrimPrefix(fake.requests[0], "GET "))
	if u.Path != "/repos/o/r/issues/1/comments" || u.Query().Get("since") != "2026-01-02T02:04:05Z" {
		t.Fatalf("request %s", fake.requests[0])
	}
}

func TestFilerRequiresAnExplicitRepositoryAndIssueNumber(t *testing.T) {
	f, fake, ctx, buf := newFiler(t)
	for _, repo := range []string{"", "o", "o/", "/r", "o/r/x", "../r", "o/.."} {
		if _, err := f.Create(ctx, repo, NewIssue{Title: "t"}); err == nil {
			t.Fatalf("%q accepted", repo)
		}
	}
	if _, err := f.Comment(ctx, "o/r", 0, "x"); err == nil {
		t.Fatal("issue number 0 accepted")
	}
	if _, err := f.State(ctx, "o/r", -1); err == nil {
		t.Fatal("negative issue number accepted")
	}
	if _, err := f.ListComments(ctx, "", 1, time.Time{}); err == nil {
		t.Fatal("empty repository accepted")
	}
	if len(fake.requests) != 0 || buf.Len() != 0 {
		t.Fatalf("invalid input must not reach GitHub: %v", fake.requests)
	}
}

// TestFilerLogsOneActivityRecordPerCall shows each call, including failures,
// no response and an unexpected status, logs one record with the action,
// access, repository, method, path, status and the reporter marker.
func TestFilerLogsOneActivityRecordPerCall(t *testing.T) {
	f, fake, ctx, buf := newFiler(t)
	fake.seedIssue(1, "open", nil, false)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	f.Create(ctx, "o/r", NewIssue{Title: "t"})
	f.Comment(ctx, "o/r", 1, "c")
	f.State(ctx, "o/r", 1)
	f.ListIssues(ctx, "o/r", since, nil)
	f.ListComments(ctx, "o/r", 1, since)
	f.Comment(ctx, "o/r", 9, "c")

	got := records(t, buf)
	// ListIssues of two issues at two per page logs two requests: the full
	// first page is followed by an empty one.
	type want struct {
		action, access, method, path string
		status                       int
		level                        string
	}
	wants := []want{
		{"issues.create", ghlog.Write, "POST", "/repos/o/r/issues", 201, "INFO"},
		{"issues.comment", ghlog.Write, "POST", "/repos/o/r/issues/1/comments", 201, "INFO"},
		{"issues.state", ghlog.Read, "GET", "/repos/o/r/issues/1", 200, "INFO"},
		{"issues.list", ghlog.Read, "GET", "/repos/o/r/issues", 200, "INFO"},
		{"issues.list", ghlog.Read, "GET", "/repos/o/r/issues", 200, "INFO"},
		{"issues.list_comments", ghlog.Read, "GET", "/repos/o/r/issues/1/comments", 200, "INFO"},
		{"issues.comment", ghlog.Write, "POST", "/repos/o/r/issues/9/comments", 201, "INFO"},
	}
	if len(got) != len(wants) {
		t.Fatalf("%d records: %+v", len(got), got)
	}
	for i, w := range wants {
		r := got[i]
		if r.Activity != ghlog.MarkerValue || r.Action != w.action || r.Access != w.access || r.Method != w.method || r.Path != w.path || r.Repository != "o/r" || r.Status != w.status || r.Level != w.level || !r.ErrorReporter {
			t.Fatalf("record %d: %+v", i, r)
		}
	}
}

func TestFilerLogsFailuresNoResponseAndUnexpectedStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		noResponse bool
		call       func(Filer, context.Context) error
		action     string
		access     string
	}{
		{"create rejected", 422, false, func(f Filer, ctx context.Context) error { _, err := f.Create(ctx, "o/r", NewIssue{}); return err }, "issues.create", ghlog.Write},
		{"create 5xx", 503, false, func(f Filer, ctx context.Context) error { _, err := f.Create(ctx, "o/r", NewIssue{}); return err }, "issues.create", ghlog.Write},
		{"comment 5xx", 500, false, func(f Filer, ctx context.Context) error { _, err := f.Comment(ctx, "o/r", 1, "x"); return err }, "issues.comment", ghlog.Write},
		{"comment unexpected 200", 200, false, func(f Filer, ctx context.Context) error { _, err := f.Comment(ctx, "o/r", 1, "x"); return err }, "issues.comment", ghlog.Write},
		{"state 403", 403, false, func(f Filer, ctx context.Context) error { _, err := f.State(ctx, "o/r", 1); return err }, "issues.state", ghlog.Read},
		{"list 502", 502, false, func(f Filer, ctx context.Context) error { _, err := f.ListIssues(ctx, "o/r", time.Time{}, nil); return err }, "issues.list", ghlog.Read},
		{"comments 404", 404, false, func(f Filer, ctx context.Context) error { _, err := f.ListComments(ctx, "o/r", 1, time.Time{}); return err }, "issues.list_comments", ghlog.Read},
		{"create no response", 0, true, func(f Filer, ctx context.Context) error { _, err := f.Create(ctx, "o/r", NewIssue{}); return err }, "issues.create", ghlog.Write},
		{"state no response", 0, true, func(f Filer, ctx context.Context) error { _, err := f.State(ctx, "o/r", 1); return err }, "issues.state", ghlog.Read},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
			f := Filer{Token: filerToken}
			if tc.noResponse {
				l, _ := net.Listen("tcp", "127.0.0.1:0")
				f.BaseURL = "http://" + l.Addr().String()
				l.Close()
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"message":"went wrong with %s"}`, filerToken)
				}))
				defer server.Close()
				f.BaseURL, f.HTTP = server.URL, server.Client()
			}
			err := tc.call(f, ctx)
			if err == nil {
				t.Fatal("expected an error")
			}
			got := records(t, &buf)
			if len(got) != 1 {
				t.Fatalf("records %+v", got)
			}
			r := got[0]
			if r.Level != "ERROR" || r.Action != tc.action || r.Access != tc.access || r.Status != tc.status || r.NoResponse != tc.noResponse || !r.ErrorReporter || r.Activity != ghlog.MarkerValue {
				t.Fatalf("record %+v", r)
			}
			if tc.noResponse && r.Outcome != "failed" {
				t.Fatalf("record %+v", r)
			}
			for _, text := range []string{buf.String(), err.Error()} {
				if strings.Contains(text, filerToken) {
					t.Fatalf("token leaked: %s", text)
				}
			}
		})
	}
}

func TestFilerWriteOutcomeIsUncertainExceptForA4xxRejection(t *testing.T) {
	writes := map[string]func(Filer, context.Context) error{
		"create":  func(f Filer, ctx context.Context) error { _, err := f.Create(ctx, "o/r", NewIssue{Title: "t"}); return err },
		"comment": func(f Filer, ctx context.Context) error { _, err := f.Comment(ctx, "o/r", 1, "x"); return err },
	}
	for name, write := range writes {
		for _, tc := range []struct {
			status            int
			uncertain, reject bool
			description       string
		}{
			{status: 500, uncertain: true, description: "500"},
			{status: 502, uncertain: true, description: "502"},
			{status: 504, uncertain: true, description: "504"},
			{status: 400, reject: true, description: "400"},
			{status: 403, reject: true, description: "403"},
			{status: 404, reject: true, description: "404"},
			{status: 422, reject: true, description: "422"},
			{status: 429, reject: true, description: "429"},
			{status: 302, uncertain: true, description: "redirect"},
			{status: 200, uncertain: true, description: "a status that is not the write's success"},
		} {
			t.Run(name+" "+tc.description, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					io.WriteString(w, `{"message":"Validation Failed"}`)
				}))
				defer server.Close()
				err := write(Filer{BaseURL: server.URL, HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, context.Background())
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("error %v", err)
				}
				if e.Status != tc.status || e.Uncertain() != tc.uncertain || IsUncertain(err) != tc.uncertain || e.Rejected() != tc.reject || IsRejected(err) != tc.reject {
					t.Fatalf("status %d uncertain %v rejected %v: %v", e.Status, e.Uncertain(), e.Rejected(), err)
				}
				if tc.reject && !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
					t.Fatalf("the error must carry the status: %v", err)
				}
			})
		}
	}
}

func TestFilerLostResponseAndTimeoutOnAWriteAreUncertain(t *testing.T) {
	// The server applies the write, then drops the connection.
	var applied int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applied++
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer server.Close()
	f := Filer{BaseURL: server.URL, Token: filerToken, HTTP: server.Client()}
	_, err := f.Create(context.Background(), "o/r", NewIssue{Title: "t"})
	var e *Error
	if !errors.As(err, &e) || !e.Uncertain() || e.Rejected() || e.Status != 0 || applied != 1 {
		t.Fatalf("lost response: %v (applied %d)", err, applied)
	}
	if _, err := f.Comment(context.Background(), "o/r", 1, "x"); !IsUncertain(err) {
		t.Fatalf("lost comment response: %v", err)
	}

	// A timeout is no response either.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	f = Filer{BaseURL: slow.URL, Token: filerToken, HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := f.Create(context.Background(), "o/r", NewIssue{Title: "t"}); !IsUncertain(err) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	f.HTTP = nil
	if _, err := f.Comment(ctx, "o/r", 1, "x"); !IsUncertain(err) {
		t.Fatalf("cancelled context: %v", err)
	}
}

func TestFilerUnreadableAnswerToAnAcceptedWriteIsUncertain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `not json`)
	}))
	defer server.Close()
	f := Filer{BaseURL: server.URL, HTTP: server.Client()}
	if _, err := f.Create(context.Background(), "o/r", NewIssue{Title: "t"}); !IsUncertain(err) {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Comment(context.Background(), "o/r", 1, "x"); !IsUncertain(err) {
		t.Fatalf("comment: %v", err)
	}
}

func TestFilerReadFailuresAreNeverUncertain(t *testing.T) {
	f, fake, ctx, _ := newFiler(t)
	fake.status, fake.message = 503, "unavailable"
	if _, err := f.State(ctx, "o/r", 1); err == nil || IsUncertain(err) {
		t.Fatalf("state: %v", err)
	}
	if _, err := f.ListIssues(ctx, "o/r", time.Time{}, nil); err == nil || IsUncertain(err) {
		t.Fatalf("list: %v", err)
	}
}

func TestFilerErrorsAndLogsNeverCarryTheTokenOrTheRequestBody(t *testing.T) {
	const bodySentinel = "sentinel-request-body-text"
	t.Setenv("GITHUB_TOKEN", filerToken)
	f, fake, ctx, buf := newFiler(t)
	// GitHub echoes the token and the body in its message.
	fake.status, fake.message = 422, "bad token "+filerToken+" for "+bodySentinel+"\nsecond line"
	var errs []error
	_, err := f.Create(ctx, "o/r", NewIssue{Title: bodySentinel, Body: bodySentinel, Labels: []string{bodySentinel}})
	errs = append(errs, err)
	_, err = f.Comment(ctx, "o/r", 1, bodySentinel)
	errs = append(errs, err)
	fake.status = 500
	_, err = f.Create(ctx, "o/r", NewIssue{Title: bodySentinel})
	errs = append(errs, err)
	for _, err := range errs {
		if err == nil {
			t.Fatal("expected an error")
		}
		text := err.Error()
		if strings.Contains(text, filerToken) || strings.Contains(text, "\n") {
			t.Fatalf("error leaks or spans lines: %q", text)
		}
	}
	// The request body does not appear in any error or record; the one
	// message that echoed it came from GitHub, not from the request.
	if strings.Contains(buf.String(), filerToken) {
		t.Fatalf("log leaked the token: %s", buf.String())
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `"github request`) && strings.Contains(line, `"title"`) {
			t.Fatalf("log carries a request body: %s", line)
		}
	}
	if strings.Contains(errs[0].Error(), "Authorization") || strings.Contains(errs[0].Error(), "Bearer") {
		t.Fatalf("error %v", errs[0])
	}

	// A token that GitHub echoes under a transport failure's text is absent too.
	noServer := Filer{BaseURL: "http://127.0.0.1:1", Token: filerToken}
	_, err = noServer.Create(ctx, "o/r", NewIssue{Title: bodySentinel})
	if err == nil || strings.Contains(err.Error(), filerToken) || strings.Contains(err.Error(), bodySentinel) {
		t.Fatalf("transport error: %v", err)
	}
}

func TestFilerRecordsCarryTheReporterMarkerAndOtherRecordsDoNot(t *testing.T) {
	f, fake, ctx, buf := newFiler(t)
	fake.seedIssue(1, "open", nil, false)
	// The caller's context is not marked; the filer's own calls are.
	if ghlog.IsReporter(ctx) {
		t.Fatal("test context must start unmarked")
	}
	if _, err := f.State(ctx, "o/r", 1); err != nil {
		t.Fatal(err)
	}
	// An existing client's record carries no marker.
	g := GitHub{BaseURL: f.BaseURL, HTTP: f.HTTP}
	fake.issues[0]["title"] = "x"
	if _, err := g.Fetch(ctx, Ref{"o", "r", 1}); err != nil {
		t.Fatal(err)
	}
	got := records(t, buf)
	if len(got) != 2 || !got[0].ErrorReporter || got[1].ErrorReporter {
		t.Fatalf("records %+v", got)
	}
	if strings.Count(buf.String(), ghlog.ReporterField) != 1 {
		t.Fatalf("log %s", buf.String())
	}
}
