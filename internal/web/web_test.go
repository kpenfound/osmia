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
)

func serve(method, path string) (*httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	return w, Serve(w, httptest.NewRequest(method, path, nil))
}

// The page and its assets are served for GET and HEAD with their content
// types and a policy that keeps the page to its own origin.
func TestServeServesThePageAndItsAssets(t *testing.T) {
	for path, want := range map[string]struct{ contentType, contains string }{
		"/":                                    {"text/html; charset=utf-8", `<script src="/app.js" defer></script>`},
		"/app.js":                              {"text/javascript; charset=utf-8", "new EventSource(api + '/events')"},
		"/style.css":                           {"text/css; charset=utf-8", "grid-template-columns"},
		"/favicon.svg":                         {"image/svg+xml", "<svg"},
		"/fonts/InstrumentSans-Variable.woff2": {"font/woff2", "wOF2"},
		"/fonts/JetBrainsMono-Variable.woff2":  {"font/woff2", "wOF2"},
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
	if !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "connect-src 'self'") || !strings.Contains(policy, "font-src 'self'") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("policy %q", policy)
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

// The two bundled fonts are the only font files, are small enough to ship
// with the page, and each sits beside its licence in the source tree.
func TestBundledFontsStayWithinTheirBudgetAndKeepTheirLicences(t *testing.T) {
	total := 0
	for path, licence := range map[string]string{
		"/fonts/InstrumentSans-Variable.woff2": "fonts/instrument-sans/OFL.txt",
		"/fonts/JetBrainsMono-Variable.woff2":  "fonts/jetbrains-mono/OFL.txt",
	} {
		w, ok := serve(http.MethodGet, path)
		if !ok {
			t.Fatalf("%s is not served", path)
		}
		total += w.Body.Len()
		text, err := assets.ReadFile(licence)
		if err != nil || !strings.Contains(string(text), "SIL Open Font License") {
			t.Fatalf("%s is not the SIL Open Font License: %v", licence, err)
		}
	}
	if total < 50_000 || total > 250_000 {
		t.Fatalf("the bundled fonts total %d bytes, want about 200 KB or less", total)
	}
	var fonts int
	for path := range paths {
		if strings.HasPrefix(path, "/fonts/") {
			fonts++
		}
	}
	if fonts != 2 {
		t.Fatalf("%d font files are served, want one sans and one mono", fonts)
	}
}

// The stylesheet is dark only and keeps to the design language's rules: one
// palette in OKLCH custom properties, no light branch, no capitals styling,
// no transition on every property, reduced motion honoured and an outline
// never removed from the composer.
func TestStylesheetKeepsToTheDesignLanguage(t *testing.T) {
	w, _ := serve(http.MethodGet, "/style.css")
	css := w.Body.String()
	root := css[strings.Index(css, ":root {"):]
	root = root[:strings.Index(root, "\n}\n")]
	if !strings.Contains(root, "color-scheme: dark") {
		t.Fatal("the palette does not declare color-scheme: dark")
	}
	if strings.Contains(css, "prefers-color-scheme") || strings.Contains(css, "color-scheme: light") {
		t.Fatal("the stylesheet has a light palette branch")
	}
	for _, banned := range []string{"text-transform", "transition: all", "outline: none", "outline:none"} {
		if strings.Contains(css, banned) {
			t.Fatalf("the stylesheet contains %q", banned)
		}
	}
	for _, want := range []string{"prefers-reduced-motion: reduce", ":focus-visible", "scrollbar-color", "select option", "font-display: swap"} {
		if !strings.Contains(css, want) {
			t.Fatalf("the stylesheet lacks %q", want)
		}
	}
	// Outside the palette block no rule holds a literal colour.
	literal := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(|\boklch\(`)
	if found := literal.FindString(strings.Replace(css, root, "", 1)); found != "" {
		t.Fatalf("a component rule holds the literal colour %q", found)
	}
	// The page itself asks for the dark scheme.
	page, _ := serve(http.MethodGet, "/")
	if !strings.Contains(page.Body.String(), `<meta name="color-scheme" content="dark">`) {
		t.Fatal("the page does not declare the dark colour scheme")
	}
}

// No element in the page repeats an attribute: a browser keeps only the first,
// so a second class list would never apply.
func TestPageElementsNeverRepeatAnAttribute(t *testing.T) {
	w, _ := serve(http.MethodGet, "/")
	tag := regexp.MustCompile(`<[a-zA-Z][^<>]*>`)
	attr := regexp.MustCompile(`\s([a-zA-Z][\w:-]*)=("[^"]*"|'[^']*')`)
	for _, el := range tag.FindAllString(w.Body.String(), -1) {
		seen := map[string]bool{}
		for _, m := range attr.FindAllStringSubmatch(el, -1) {
			if seen[m[1]] {
				t.Fatalf("element repeats the %s attribute: %s", m[1], el)
			}
			seen[m[1]] = true
		}
	}
}

// The four create forms share one layout: the form class, fields with a
// label tied to their control, and the actions after the last field.
func TestCreateFormsShareOneLayoutInTheMarkup(t *testing.T) {
	w, _ := serve(http.MethodGet, "/")
	page := w.Body.String()
	for _, id := range []string{"handin-form", "design-add-form", "session-add-form", "project-add"} {
		start := strings.Index(page, `<form id="`+id+`" class="form"`)
		if start < 0 {
			t.Fatalf("form %s does not use the shared form layout", id)
		}
		form := page[start : start+strings.Index(page[start:], "</form>")]
		if !strings.Contains(form, `<div class="form-actions">`) || !strings.Contains(form, `<p class="result" role="status"></p>`) {
			t.Fatalf("form %s lacks its actions or result", id)
		}
		if strings.Index(form, `class="form-actions"`) < strings.LastIndex(form, `class="field"`) {
			t.Fatalf("form %s puts its actions before a field", id)
		}
		for _, label := range regexp.MustCompile(`<label for="([^"]+)"`).FindAllStringSubmatch(form, -1) {
			if !strings.Contains(form, `id="`+label[1]+`"`) {
				t.Fatalf("form %s has a label for %s with no control", id, label[1])
			}
		}
		if strings.Contains(form, "<label>") {
			t.Fatalf("form %s has a label that wraps its control", id)
		}
	}
	if strings.Contains(page, "window.confirm") {
		t.Fatal("the page markup asks the browser to confirm")
	}
}

// The script never asks the browser to confirm anything: the actions that
// need confirming open the page's own dialog.
func TestScriptNeverUsesTheNativeConfirmation(t *testing.T) {
	w, _ := serve(http.MethodGet, "/app.js")
	for _, banned := range []string{"window.confirm", "confirm(", "alert(", "prompt("} {
		if strings.Contains(w.Body.String(), banned) {
			t.Fatalf("the script calls %s", banned)
		}
	}
}
