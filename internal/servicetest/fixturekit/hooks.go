package fixturekit

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// HooksPost is one request a fake Hearsay connectors listener received.
type HooksPost struct {
	Source string
	Auth   string
	Raw    string
	Body   map[string]any
}

// Str returns a string field of the post's body, empty when it has none.
func (p HooksPost) Str(field string) string { s, _ := p.Body[field].(string); return s }

// Hooks stands in for the connectors listener of Hearsay's work source. It
// checks nothing of Hearsay's own validation beyond the deciders a decision may
// name; it records the requests a publisher makes and accepts them all unless
// told to answer with another status.
type Hooks struct {
	*httptest.Server
	mu     sync.Mutex
	posts  []HooksPost
	status int
}

// NewHooks starts a fake listener that is closed when the test ends.
func NewHooks(t *testing.T) *Hooks { return NewHooksOn(t, "127.0.0.1:0") }

// NewHooksOn starts a fake listener on addr that is closed when the test ends.
func NewHooksOn(t *testing.T, addr string) *Hooks {
	t.Helper()
	h := &Hooks{status: http.StatusAccepted}
	listener, err := net.Listen("tcp", addr)
	Must(t, err)
	h.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		post := HooksPost{Source: strings.TrimPrefix(r.URL.Path, "/hooks/"), Auth: r.Header.Get("Authorization"), Raw: string(raw)}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/hooks/") || json.Unmarshal(raw, &post.Body) != nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if post.Body["kind"] == "decision" && !slices.Contains([]string{"owner", trace.Manager, "committee"}, post.Str("decider")) {
			t.Errorf("a decision carries decider %q, which Hearsay's connector contract refuses: %s", post.Str("decider"), raw)
		}
		h.mu.Lock()
		status := h.status
		if status == http.StatusAccepted {
			h.posts = append(h.posts, post)
		}
		h.mu.Unlock()
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			_, _ = w.Write([]byte(`{"id":"evt:fake"}`))
		}
	}))
	h.Server.Listener.Close()
	h.Server.Listener = listener
	h.Server.Start()
	t.Cleanup(h.Server.Close)
	return h
}

// SetStatus makes the listener answer with status; only accepted posts are
// recorded.
func (h *Hooks) SetStatus(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = status
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
