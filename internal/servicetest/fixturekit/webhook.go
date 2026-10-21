package fixturekit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

type Webhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	// answers holds the status each post in bodies was answered with.
	answers []int
	status  int
	// ledger, when set, is the notification ledger that must hold each
	// posted body as pending before the post arrives.
	ledger string
}

func NewWebhook(t *testing.T) *Webhook {
	t.Helper()
	w := &Webhook{status: http.StatusOK}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/hook/secret" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("request %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if w.ledger != "" && !pendingIn(w.ledger, string(body)) {
			t.Errorf("posted before the ledger recorded it as pending: %q", body)
		}
		w.bodies = append(w.bodies, string(body))
		w.answers = append(w.answers, w.status)
		rw.WriteHeader(w.status)
	}))
	t.Cleanup(w.Close)
	return w
}

// pendingIn reports whether the ledger at path holds body as a pending
// notification.
func pendingIn(path, body string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var l struct {
		Notifications map[string]struct {
			State string
			Body  string
		}
	}
	if json.Unmarshal(data, &l) != nil {
		return false
	}
	for _, rec := range l.Notifications {
		if rec.State == "pending" && rec.Body == body {
			return true
		}
	}
	return false
}

// Records makes the webhook check that each post is recorded as pending in
// the ledger of opts' root first.
func (w *Webhook) Records(root string) *Webhook {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ledger = filepath.Join(root, "notifications.json")
	return w
}

func (w *Webhook) Hook() string { return w.URL + "/hook/secret" }

func (w *Webhook) Respond(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}

func (w *Webhook) Received() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.bodies)
}

// Accepted returns the posts the webhook answered with a success status.
func (w *Webhook) Accepted() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for i, body := range w.bodies {
		if w.answers[i] < 300 {
			out = append(out, body)
		}
	}
	return out
}
