package fixturekit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// HooksPost is one request a fake Hearsay connectors listener received.
type HooksPost struct {
	// Source is the Hearsay source the request was posted to.
	Source string
	// Raw is the request body as sent.
	Raw string
	// Body is the request body decoded.
	Body map[string]any
}

// Str returns a string field of the post's body, empty when it has none.
func (p HooksPost) Str(field string) string { s, _ := p.Body[field].(string); return s }

// Hooks stands in for the connectors listener of Hearsay's work source. It
// checks nothing of Hearsay's own validation beyond the deciders a decision may
// name; it records the requests a publisher makes and accepts them all.
type Hooks struct {
	*httptest.Server
	mu    sync.Mutex
	posts []HooksPost
}

// NewHooks starts a fake listener that is closed when the test ends.
func NewHooks(t *testing.T) *Hooks {
	t.Helper()
	h := &Hooks{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		post := HooksPost{Source: strings.TrimPrefix(r.URL.Path, "/hooks/"), Raw: string(raw)}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/hooks/") || json.Unmarshal(raw, &post.Body) != nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		// Hearsay answers 400 to a decision whose decider is missing or is not
		// one of these.
		if post.Body["kind"] == "decision" && !slices.Contains([]string{"owner", "chief_of_staff", "committee"}, post.Str("decider")) {
			t.Errorf("a decision carries decider %q, which Hearsay's connector contract refuses: %s", post.Str("decider"), raw)
		}
		h.mu.Lock()
		h.posts = append(h.posts, post)
		h.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt:fake"}`))
	}))
	t.Cleanup(h.Server.Close)
	return h
}

// Take returns the requests received since the last take.
func (h *Hooks) Take() []HooksPost {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.posts
	h.posts = nil
	return out
}

// Decisions returns the posts of kind decision.
func Decisions(posts []HooksPost) []HooksPost {
	var out []HooksPost
	for _, p := range posts {
		if p.Str("kind") == "decision" {
			out = append(out, p)
		}
	}
	return out
}
