package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(method, path string) (*httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	return w, Serve(w, httptest.NewRequest(method, path, nil))
}

// The page and its assets are served for GET and HEAD with their content
// types and a policy that keeps the page to its own origin.
func TestServeServesThePageAndItsAssets(t *testing.T) {
	for path, want := range map[string]struct{ contentType, contains string }{
		"/":          {"text/html; charset=utf-8", `<script src="/app.js" defer></script>`},
		"/app.js":    {"text/javascript; charset=utf-8", "new EventSource(api + '/events')"},
		"/style.css": {"text/css; charset=utf-8", "grid-template-columns"},
	} {
		w, ok := serve(http.MethodGet, path)
		if !ok || w.Code != http.StatusOK {
			t.Fatalf("GET %s: served %v, status %d", path, ok, w.Code)
		}
		h := w.Header()
		if h.Get("Content-Type") != want.contentType || h.Get("Content-Security-Policy") != policy || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-cache" {
			t.Fatalf("GET %s headers: %v", path, h)
		}
		if !strings.Contains(w.Body.String(), want.contains) {
			t.Fatalf("GET %s body lacks %q", path, want.contains)
		}
		head, ok := serve(http.MethodHead, path)
		if !ok || head.Code != http.StatusOK || head.Header().Get("Content-Length") != h.Get("Content-Length") {
			t.Fatalf("HEAD %s: served %v, status %d, headers %v", path, ok, head.Code, head.Header())
		}
	}
	if !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "connect-src 'self'") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("policy %q", policy)
	}
}

// Other methods and paths are left to the caller.
func TestServeLeavesOtherRequests(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/"},
		{http.MethodPut, "/app.js"},
		{http.MethodGet, "/index.html"},
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/web.go"},
	} {
		if w, ok := serve(c.method, c.path); ok || w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Fatalf("%s %s was served: %d %v", c.method, c.path, w.Code, w.Header())
		}
	}
}
