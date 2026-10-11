package pulls

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/ghlog"
)

func TestGitHubFindsAndCreatesPullRequestsWithTheToken(t *testing.T) {
	t.Parallel()
	var created map[string]string
	// server stands in for the real GitHub REST API: a remote system whose
	// real responses and rate limits this fake does not reproduce.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("headers %v", r.Header)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls":
			if q := r.URL.Query(); q.Get("head") != "owner:osmia/w" || q.Get("state") != "all" {
				t.Errorf("query %v", q)
			}
			io.WriteString(w, `[
				{"number": 7, "html_url": "https://github.com/acme/widgets/pull/7", "state": "open", "title": "T", "body": "B", "head": {"ref": "osmia/w", "sha": "abc", "repo": {"full_name": "owner/widgets"}}, "base": {"ref": "main"}},
				{"number": 8, "html_url": "https://github.com/acme/widgets/pull/8", "state": "closed", "merged_at": "2026-01-01T00:00:00Z", "title": "T", "body": null, "head": {"ref": "osmia/w", "sha": "def", "repo": {"full_name": "owner/gadgets"}}, "base": {"ref": "main"}},
				{"number": 9, "html_url": "https://github.com/acme/widgets/pull/9", "state": "closed", "title": "T", "body": null, "head": {"ref": "osmia/w", "sha": "def", "repo": null}, "base": {"ref": "main"}}
			]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/pulls":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number": 10, "html_url": "https://github.com/acme/widgets/pull/10", "state": "open", "title": "Title", "body": "Body", "head": {"ref": "osmia/w", "sha": "abc", "repo": {"full_name": "owner/widgets"}}, "base": {"ref": "main"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
	ctx := context.Background()
	found, err := g.Find(ctx, "acme/widgets", "owner/widgets", "osmia/w")
	if err != nil || len(found) != 1 {
		t.Fatalf("found %+v: %v", found, err)
	}
	want := PullRequest{Number: 7, URL: "https://github.com/acme/widgets/pull/7", State: "open", Head: "osmia/w", HeadRepository: "owner/widgets", HeadCommit: "abc", Base: "main", Title: "T", Body: "B"}
	if found[0] != want {
		t.Fatalf("found %+v, want %+v", found[0], want)
	}
	pr, err := g.Create(ctx, "acme/widgets", New{HeadRepository: "owner/widgets", Head: "osmia/w", Base: "main", Title: "Title", Body: "Body"})
	if err != nil || pr.Number != 10 || pr.HeadCommit != "abc" || pr.URL != "https://github.com/acme/widgets/pull/10" {
		t.Fatalf("created %+v: %v", pr, err)
	}
	if created["head"] != "owner:osmia/w" || created["base"] != "main" || created["title"] != "Title" || created["body"] != "Body" {
		t.Fatalf("sent %v", created)
	}
	if _, err := g.Find(ctx, "acme/gadgets", "owner/widgets", "osmia/w"); err == nil || !strings.Contains(err.Error(), "GitHub answered 404") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a missing repository: %v", err)
	}
}

func TestGitHubSameRepositoryCreateAndRetarget(t *testing.T) {
	// server stands in for the real GitHub REST API, the same remote
	// boundary noted above.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing service credentials")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/repos/owner/repo/pulls" || body["head"] != "osmia/child" || body["base"] != "osmia/parent" {
				t.Errorf("create %s %v", r.URL.Path, body)
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodPatch:
			if r.URL.Path != "/repos/owner/repo/pulls/7" || body["base"] != "main" || body["body"] != "Approved" || body["title"] != "Ready" {
				t.Errorf("update %s %v", r.URL.Path, body)
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		io.WriteString(w, `{"number":7,"state":"open","base":{"ref":"`+body["base"]+`"}}`)
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, HTTP: server.Client(), Token: "secret"}
	pr, err := g.Create(context.Background(), "owner/repo", New{HeadRepository: "owner/repo", Head: "osmia/child", Base: "osmia/parent"})
	if err != nil || pr.Number != 7 || pr.Base != "osmia/parent" {
		t.Fatalf("create %+v %v", pr, err)
	}
	pr, err = g.Update(context.Background(), "owner/repo", 7, Update{Base: "main", Title: "Ready", Body: "Approved"})
	if err != nil || pr.Number != 7 || pr.Base != "main" {
		t.Fatalf("update %+v %v", pr, err)
	}
}

func TestGitHubErrorsKeepGitHubsExplanation(t *testing.T) {
	t.Parallel()
	// server stands in for the real GitHub REST API, the same remote
	// boundary noted above.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/permissions/pulls":
			w.Header().Set("X-Accepted-GitHub-Permissions", "pull_requests=read")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message": "Resource not accessible by personal access token\nsecond line", "documentation_url": "https://docs.github.com/rest/pulls/pulls#list-pull-requests"}`)
		case "/repos/acme/limited/pulls":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1791079426")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message": "`+strings.Repeat("x", 600)+`"}`)
		default:
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `<html>bad gateway</html>`)
		}
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
	ctx := context.Background()
	_, err := g.Find(ctx, "acme/permissions", "acme/permissions", "osmia/w")
	want := "find pull requests of acme/permissions from acme/permissions:osmia/w: GitHub answered 403: Resource not accessible by personal access token second line; the request needs the token permissions pull_requests=read; see https://docs.github.com/rest/pulls/pulls#list-pull-requests"
	if err == nil || err.Error() != want {
		t.Fatalf("a refused token:\n got %v\nwant %s", err, want)
	}
	_, err = g.Find(ctx, "acme/limited", "acme/limited", "osmia/w")
	if err == nil || !strings.Contains(err.Error(), strings.Repeat("x", 500)+"…;") || strings.Contains(err.Error(), strings.Repeat("x", 501)) ||
		!strings.Contains(err.Error(), "; the rate limit is exhausted until 2026-10-04T02:03:46Z; GitHub asks to retry after 60s") {
		t.Fatalf("an exhausted rate limit: %v", err)
	}
	_, err = g.Find(ctx, "acme/gateway", "acme/gateway", "osmia/w")
	if err == nil || !strings.HasSuffix(err.Error(), ": GitHub answered 502") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a body that is not GitHub's JSON: %v", err)
	}
}

type silentClient struct{ Client }

func TestCredentialed(t *testing.T) {
	t.Parallel()
	if Credentialed(GitHub{}) || !Credentialed(GitHub{Token: "secret"}) {
		t.Fatal("GitHub is credentialed exactly when it has a token")
	}
	if Credentialed(nil) || !Credentialed(silentClient{}) {
		t.Fatal("no client holds no credential, and a client that does not say is assumed to")
	}
}

// ghRecord is one decoded GitHub activity log line.
type ghRecord struct {
	Activity   string `json:"activity"`
	Access     string `json:"access"`
	Action     string `json:"action"`
	Repository string `json:"repository"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	NoResponse bool   `json:"no_response"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error"`
	Level      string `json:"level"`
}

// decodeGHRecords parses every line of buf as one JSON object, failing the
// test if any line does not parse: every line a logger writes must be valid
// JSON with a level and a message.
func decodeGHRecords(t *testing.T, buf *bytes.Buffer) []ghRecord {
	t.Helper()
	var records []ghRecord
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r struct {
			ghRecord
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.Level == "" || r.Msg == "" {
			t.Fatalf("line does not parse as a structured record: %s: %v", line, err)
		}
		records = append(records, r.ghRecord)
	}
	return records
}

// TestGitHubLogsOneRecordPerRequest shows one GitHub activity record for
// each of Find, Create and Update, on both success and failure, with the
// action, method, path, repository, status and the marker that tells them
// apart from any other kind of record.
func TestGitHubLogsOneRecordPerRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls":
			io.WriteString(w, `[{"number":7,"html_url":"u","state":"open","head":{"ref":"osmia/w","sha":"abc","repo":{"full_name":"owner/widgets"}},"base":{"ref":"main"}}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/pulls":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":8,"state":"open","head":{"ref":"osmia/w","sha":"abc"},"base":{"ref":"main"}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widgets/pulls/8":
			io.WriteString(w, `{"number":8,"state":"open","head":{"ref":"osmia/w","sha":"abc"},"base":{"ref":"main"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: "p_1", Workstream: "w_1"})

	if _, err := g.Find(ctx, "acme/widgets", "owner/widgets", "osmia/w"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Create(ctx, "acme/widgets", New{HeadRepository: "owner/widgets", Head: "osmia/w", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Update(ctx, "acme/widgets", 8, Update{Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Find(ctx, "acme/missing", "owner/widgets", "osmia/w"); err == nil {
		t.Fatal("expected a 404")
	}
	// A transport failure: no response arrives at all.
	broken := GitHub{BaseURL: "http://127.0.0.1:1", Token: "secret", HTTP: server.Client()}
	if _, err := broken.Find(ctx, "acme/widgets", "owner/widgets", "osmia/w"); err == nil {
		t.Fatal("expected a transport failure")
	}

	records := decodeGHRecords(t, &buf)
	if len(records) != 5 {
		t.Fatalf("records %+v", records)
	}
	want := []struct {
		action, access, method string
		status                 int
		noResponse             bool
	}{
		{"pulls.find", ghlog.Read, http.MethodGet, http.StatusOK, false},
		{"pulls.create", ghlog.Write, http.MethodPost, http.StatusCreated, false},
		{"pulls.update", ghlog.Write, http.MethodPatch, http.StatusOK, false},
		{"pulls.find", ghlog.Read, http.MethodGet, http.StatusNotFound, false},
		{"pulls.find", ghlog.Read, http.MethodGet, 0, true},
	}
	for i, r := range records {
		w := want[i]
		if r.Activity != ghlog.MarkerValue || r.Action != w.action || r.Access != w.access || r.Method != w.method || r.Status != w.status || r.NoResponse != w.noResponse {
			t.Fatalf("record %d: %+v, want %+v", i, r, w)
		}
		if r.Repository != "acme/widgets" && r.Repository != "acme/missing" {
			t.Fatalf("record %d missing its repository: %+v", i, r)
		}
		if i >= 3 && (r.Outcome != "failed" || r.Error == "" || r.Level != "ERROR") {
			t.Fatalf("failed request %d did not log its failure at Error: %+v", i, r)
		}
		if i < 3 && (r.Outcome != "ok" || r.Error != "" || r.Level != "INFO") {
			t.Fatalf("successful request %d: %+v", i, r)
		}
		if strings.Contains(r.Error, "secret") {
			t.Fatalf("record %d leaked the token: %+v", i, r)
		}
	}
}

// alwaysFailingWriter never writes successfully, standing in for a broken
// log destination.
type alwaysFailingWriter struct{}

func (alwaysFailingWriter) Write([]byte) (int, error) { return 0, errBrokenWriter }

var errBrokenWriter = errors.New("broken log writer")

// TestGitHubActionSucceedsEvenWhenTheLogWriterFails shows that a failing log
// destination never changes the result of the GitHub action it would have
// logged.
func TestGitHubActionSucceedsEvenWhenTheLogWriterFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"number":7,"html_url":"u","state":"open","head":{"ref":"osmia/w","sha":"abc","repo":{"full_name":"owner/widgets"}},"base":{"ref":"main"}}]`)
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(alwaysFailingWriter{}, nil)))
	found, err := g.Find(ctx, "acme/widgets", "owner/widgets", "osmia/w")
	if err != nil || len(found) != 1 || found[0].Number != 7 {
		t.Fatalf("a failing log writer changed the action's result: %+v %v", found, err)
	}
}

// TestGitHubLogNeverCarriesSecretsOrPullRequestBodies shows that creating and
// updating a pull request with a title and body, while every known secret
// environment variable holds a sentinel value, logs neither those sentinels
// nor the title or body text: the GitHub activity record carries only the
// action, method, path, status and outcome, never the request it sent.
func TestGitHubLogNeverCarriesSecretsOrPullRequestBodies(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "sentinel-github-token")
	t.Setenv("TS_AUTHKEY", "sentinel-ts-authkey")
	t.Setenv("OPENROUTER_API_KEY", "sentinel-jev-key")
	const title = "sentinel pull request title"
	const body = "sentinel pull request body text"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":8,"state":"open","head":{"ref":"osmia/w","sha":"abc"},"base":{"ref":"main"}}`)
		default:
			io.WriteString(w, `{"number":8,"state":"open","head":{"ref":"osmia/w","sha":"abc"},"base":{"ref":"main"}}`)
		}
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "sentinel-github-token", HTTP: server.Client()}
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if _, err := g.Create(ctx, "acme/widgets", New{HeadRepository: "owner/widgets", Head: "osmia/w", Base: "main", Title: title, Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Update(ctx, "acme/widgets", 8, Update{Title: title, Body: body}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, sentinel := range []string{"sentinel-github-token", "sentinel-ts-authkey", "sentinel-jev-key", title, body} {
		if strings.Contains(out, sentinel) {
			t.Fatalf("log leaked %q: %s", sentinel, out)
		}
	}
}
