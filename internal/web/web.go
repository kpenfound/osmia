// Package web holds the page the service serves beside its API. The page is
// a client of the /v1 API like the command line: it reads the views and the
// event stream, acts through the same endpoints, and needs no handler of its
// own.
package web

import (
	"embed"
	"net/http"
	"strconv"
)

//go:embed index.html app.js style.css
var assets embed.FS

// asset is one embedded file and the content type it is served with.
type asset struct {
	name        string
	contentType string
}

// paths maps each URL path the page is served under to its file.
var paths = map[string]asset{
	"/":          {"index.html", "text/html; charset=utf-8"},
	"/app.js":    {"app.js", "text/javascript; charset=utf-8"},
	"/style.css": {"style.css", "text/css; charset=utf-8"},
}

// policy lets the page load only its own script and style and talk only to
// its own origin, so no other site can frame it and no injected markup can
// run. The empty data: icon keeps the browser from asking for a favicon.
const policy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Serve writes the page file at r's path for a GET or HEAD request and
// reports whether it did; it writes nothing for any other request.
func Serve(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	file, ok := paths[r.URL.Path]
	if !ok {
		return false
	}
	content, err := assets.ReadFile(file.name)
	if err != nil {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", file.contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", policy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	w.Write(content)
	return true
}
