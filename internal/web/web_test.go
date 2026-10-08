package web

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func serve(method, path string) (*httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	return w, Serve(w, httptest.NewRequest(method, path, nil))
}

// The page and its assets are served for GET and HEAD with their content
// types and a policy that keeps the page to its own origin.
func TestServeServesThePageAndItsAssets(t *testing.T) {
	for path, want := range map[string]struct{ contentType, contains string }{
		"/":            {"text/html; charset=utf-8", `<script src="/app.js" defer></script>`},
		"/app.js":      {"text/javascript; charset=utf-8", "new EventSource(api + '/events')"},
		"/style.css":   {"text/css; charset=utf-8", "grid-template-columns"},
		"/favicon.svg": {"image/svg+xml", "<svg"},
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

// The page never names the Beekeeper's shadow project or its reserved
// workstream: it reaches the Beekeeper only through the service-level
// Beekeeper endpoints, with no special case naming the shadow identity.
func TestAssetsNeverReferenceTheShadowProjectIdentifier(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "style.css", "favicon.svg"} {
		content, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), string(config.ShadowProjectID)) || strings.Contains(string(content), string(config.BeekeeperWorkstreamID)) {
			t.Fatalf("%s references the beekeeper's shadow project identifier", name)
		}
	}
}

// The page declares exactly one SVG favicon in its head, and fetching it
// returns the bee emoji with no reference to anything outside the service.
func TestPageDeclaresABeeFaviconServedAsSVG(t *testing.T) {
	w, ok := serve(http.MethodGet, "/")
	if !ok || w.Code != http.StatusOK {
		t.Fatalf("GET /: served %v, status %d", ok, w.Code)
	}
	page := w.Body.String()
	head := page
	if i, j := strings.Index(page, "<head>"), strings.Index(page, "</head>"); i >= 0 && j > i {
		head = page[i:j]
	} else {
		t.Fatalf("page has no <head>...</head>: %s", page)
	}
	links := regexp.MustCompile(`<link\s+rel="icon"[^>]*>`).FindAllString(head, -1)
	if len(links) != 1 {
		t.Fatalf("head has %d <link rel=\"icon\"> elements, want 1: %v", len(links), links)
	}
	link := links[0]
	if !strings.Contains(link, `type="image/svg+xml"`) {
		t.Fatalf("icon link lacks type=\"image/svg+xml\": %s", link)
	}
	href := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(link)
	if href == nil {
		t.Fatalf("icon link has no href: %s", link)
	}
	if strings.Contains(href[1], "://") {
		t.Fatalf("icon href points off the service: %s", href[1])
	}

	icon, ok := serve(http.MethodGet, href[1])
	if !ok || icon.Code != http.StatusOK {
		t.Fatalf("GET %s: served %v, status %d", href[1], ok, icon.Code)
	}
	if ct := icon.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("GET %s content type %q, want image/svg+xml", href[1], ct)
	}
	body := icon.Body.Bytes()
	dec := xml.NewDecoder(icon.Body)
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("icon body is not well-formed XML: %v", err)
		}
	}
	if !strings.Contains(string(body), "\U0001F41D") && !strings.Contains(string(body), "&#x1F41D;") {
		t.Fatalf("icon body has no bee: %s", body)
	}
	// A standalone image/svg+xml document still needs its xmlns declaration to
	// render, so the external-reference check walks tokens for actual
	// resource references (href/xlink:href/src, url(), @import) instead of
	// matching "http://" as a raw substring of the body.
	dec = xml.NewDecoder(bytes.NewReader(body))
	inStyle := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("icon body is not well-formed XML: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if tok.Name.Local == "style" {
				inStyle = true
			}
			for _, attr := range tok.Attr {
				if attr.Name.Space == "xmlns" || attr.Name.Local == "xmlns" {
					continue
				}
				switch attr.Name.Local {
				case "href", "src":
					if !strings.HasPrefix(attr.Value, "#") {
						t.Fatalf("icon body references an external resource via %s: %s", attr.Name.Local, attr.Value)
					}
				case "style":
					if strings.Contains(attr.Value, "url(") || strings.Contains(attr.Value, "@import") {
						t.Fatalf("icon body references an external resource in style attribute: %s", attr.Value)
					}
				}
			}
		case xml.EndElement:
			if tok.Name.Local == "style" {
				inStyle = false
			}
		case xml.CharData:
			if inStyle && (strings.Contains(string(tok), "url(") || strings.Contains(string(tok), "@import")) {
				t.Fatalf("icon body references an external resource in <style>: %s", tok)
			}
		}
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
