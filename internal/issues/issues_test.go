package issues

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
