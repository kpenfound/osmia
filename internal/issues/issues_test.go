package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/ghlog"
)

func TestParse(t *testing.T) {
	ref, err := Parse("https://github.com/kpenfound/osmia/issues/80")
	if err != nil || ref != (Ref{"kpenfound", "osmia", 80}) || ref.URL() != "https://github.com/kpenfound/osmia/issues/80" {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, raw := range []string{
		"http://github.com/a/b/issues/1", "https://example.com/a/b/issues/1", "https://github.com/a/b/pull/1",
		"https://github.com/a/b/issues/0", "https://github.com/a/b/issues/01", "https://github.com/a/b/issues/x",
		"https://github.com/a/b/issues/1/", "https://github.com/a/b/issues/1?x=1", "https://github.com/a/b/issues/1#c",
		"https://user@github.com/a/b/issues/1", "https://github.com/../b/issues/1", "https://github.com/a%2Fc/b/issues/1",
		"https://github.com/a/b/c/issues/1", "design.md", "",
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestRender(t *testing.T) {
	for _, c := range []struct{ title, body, want string }{
		{"Title", "", "# Title\n"},
		{"Title", "Body", "# Title\n\nBody\n"},
		{"Title", "Body\n", "# Title\n\nBody\n"},
	} {
		if got := Render(c.title, c.body); got != c.want {
			t.Errorf("Render(%q, %q) = %q", c.title, c.body, got)
		}
	}
}

func TestGitHubFetch(t *testing.T) {
	var auth, path string
	answer := `{"title":"Add hand-in","body":"Design text"}`
	status := http.StatusOK
	// server stands in for the real GitHub REST API: a remote, rate-limited
	// system. It leaves unverified whether the real API's pagination, rate
	// limiting and exact response shapes match what this fake returns.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(status)
		w.Write([]byte(answer))
	}))
	defer server.Close()
	ctx := context.Background()
	ref := Ref{"owner", "repo", 7}
	g := GitHub{BaseURL: server.URL + "/", Token: "secret"}
	text, err := g.Fetch(ctx, ref)
	if err != nil || text != "# Add hand-in\n\nDesign text\n" || auth != "Bearer secret" || path != "/repos/owner/repo/issues/7" {
		t.Fatalf("%q %v %q %q", text, err, auth, path)
	}
	answer = `{"title":"No body","body":null}`
	if text, err := (GitHub{BaseURL: server.URL}).Fetch(ctx, ref); err != nil || text != "# No body\n" || auth != "" {
		t.Fatalf("%q %v %q", text, err, auth)
	}
	for _, c := range []struct {
		status int
		answer string
	}{
		{http.StatusNotFound, `{"message":"Not Found"}`},
		{http.StatusOK, `{"body":"no title"}`},
		{http.StatusOK, `not json`},
		{http.StatusOK, `{"title":"A PR","pull_request":{}}`},
		{http.StatusOK, `{"title":"` + strings.Repeat("x", MaxResponse) + `"}`},
	} {
		status, answer = c.status, c.answer
		_, err := g.Fetch(ctx, ref)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("%d %.40s: %v", c.status, c.answer, err)
		}
	}
}

// TestGitHubFetchLogsOneGitHubActivityRecordPerCall shows one record for a
// successful fetch and one for a failing one, each with the marker that
// tells GitHub activity apart from any other kind of record, the action,
// method, path, repository and status, and no token in the error.
func TestGitHubFetchLogsOneGitHubActivityRecordPerCall(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, `{"title":"Add hand-in","body":"Design text"}`)
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	ref := Ref{"owner", "repo", 7}

	if _, err := g.Fetch(ctx, ref); err != nil {
		t.Fatal(err)
	}
	status = http.StatusNotFound
	if _, err := g.Fetch(ctx, ref); err == nil {
		t.Fatal("expected a 404")
	}

	var records []struct {
		Activity, Access, Action, Repository, Method, Path, Level, Error string
		Status                                                           int
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var r struct {
			Activity, Access, Action, Repository, Method, Path, Level, Error string
			Status                                                           int
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line does not parse as JSON: %s: %v", line, err)
		}
		records = append(records, r)
	}
	if len(records) != 2 {
		t.Fatalf("records %+v", records)
	}
	for i, want := range []struct {
		status int
		level  string
	}{{http.StatusOK, "INFO"}, {http.StatusNotFound, "ERROR"}} {
		r := records[i]
		if r.Activity != ghlog.MarkerValue || r.Action != "issues.fetch" || r.Access != ghlog.Read || r.Method != http.MethodGet ||
			r.Path != "/repos/owner/repo/issues/7" || r.Repository != "owner/repo" || r.Status != want.status || r.Level != want.level {
			t.Fatalf("record %d: %+v", i, r)
		}
		if strings.Contains(r.Error, "secret") {
			t.Fatalf("record %d leaked the token: %+v", i, r)
		}
	}
}

// TestGitHubFetchLogNeverCarriesSecretsOrIssueBody shows that fetching an
// issue whose title and body hold sentinel text, while every known secret
// environment variable holds a sentinel value, logs neither those sentinels
// nor the issue's title or body: the GitHub activity record carries only the
// action, method, path, status and outcome, never the issue's content.
func TestGitHubFetchLogNeverCarriesSecretsOrIssueBody(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "sentinel-github-token")
	t.Setenv("TS_AUTHKEY", "sentinel-ts-authkey")
	t.Setenv("OPENROUTER_API_KEY", "sentinel-jev-key")
	const title = "sentinel issue title"
	const body = "sentinel issue body text"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"title":"`+title+`","body":"`+body+`"}`)
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: "sentinel-github-token", HTTP: server.Client()}
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if _, err := g.Fetch(ctx, Ref{"owner", "repo", 7}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, sentinel := range []string{"sentinel-github-token", "sentinel-ts-authkey", "sentinel-jev-key", title, body} {
		if strings.Contains(out, sentinel) {
			t.Fatalf("log leaked %q: %s", sentinel, out)
		}
	}
}
