package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// routeSamples fills each path parameter of Routes with a well-formed value.
var routeSamples = strings.NewReplacer(
	"{workstream}", string(stream),
	"{project}", string(project),
	"{number}", "1",
	"{unit}", "u1",
	"{amendment}", "1",
	"{question}", "1",
	"{criterion}", "spec#1",
	"{commit}", strings.Repeat("0", 40),
)

// TestRoutesAreServed checks that the handler serves every route in Routes:
// an unmatched method and path gets unsupported, anything else is served.
func TestRoutesAreServed(t *testing.T) {
	t.Parallel()
	s, _ := start(t, fixture(t))
	seen := map[string]bool{}
	for _, route := range Routes {
		key := route.Method + " " + route.Path
		if seen[key] {
			t.Errorf("%s is listed twice", key)
		}
		seen[key] = true
		if !strings.HasPrefix(route.Path, Prefix+"/") || route.Summary == "" || route.Response == nil {
			t.Errorf("%s needs a %s path, a summary and a response", key, Prefix)
		}
		// Stopping would end the service the other routes are checked on.
		if route.Path == Prefix+"/stop" {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		// A cancelled request ends the event stream after its first event.
		cancel()
		body := ""
		if route.Request != nil {
			// A body that is not JSON keeps an operation from running.
			body = "not json"
		}
		req := httptest.NewRequestWithContext(ctx, route.Method, routeSamples.Replace(route.Path), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handle(rec, req)
		var failure ErrorResponse
		if rec.Code == http.StatusNotImplemented && json.Unmarshal(rec.Body.Bytes(), &failure) == nil && failure.Error.Code == Unsupported {
			t.Errorf("%s is listed in Routes but not served", key)
		}
	}
}

// TestRoutesListEveryPath checks that every API path the service's handlers
// match is listed in Routes, so the OpenAPI description covers it.
func TestRoutesListEveryPath(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	must(t, err)
	literal := regexp.MustCompile(`Prefix\s*\+\s*"(/[^"]*)"`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "client.go" {
			continue
		}
		src, err := os.ReadFile(file)
		must(t, err)
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			path := Prefix + m[1]
			listed := false
			for _, route := range Routes {
				if route.Path == path || strings.HasSuffix(path, "/") && strings.HasPrefix(route.Path, path) {
					listed = true
					break
				}
			}
			if !listed {
				t.Errorf("%s matches %s, which Routes does not list", file, path)
			}
		}
	}
}
