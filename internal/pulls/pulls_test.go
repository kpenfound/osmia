package pulls

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubFindsAndCreatesPullRequestsWithTheToken(t *testing.T) {
	t.Parallel()
	var created map[string]string
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
