package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

var routeSamples = strings.NewReplacer(
	"{workstream}", string(fixturekit.Stream),
	"{project}", string(fixturekit.Project),
	"{number}", "1",
	"{unit}", "u1",
	"{image}", "image-unknown",
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
		if route.Path == Prefix+"/stop" {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		body := ""
		if route.Request != nil {
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

func TestRoutesListEveryPath(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	fixturekit.Must(t, err)
	literal := regexp.MustCompile(`Prefix\s*\+\s*"(/[^"]*)"`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "client.go" {
			continue
		}
		src, err := os.ReadFile(file)
		fixturekit.Must(t, err)
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

func TestApproveAndLandRouteMatchesItsHandler(t *testing.T) {
	t.Parallel()
	path := Prefix + "/delivery/{workstream}/land"
	var route *Route
	for i := range Routes {
		if Routes[i].Path == path {
			route = &Routes[i]
		}
	}
	if route == nil || route.Method != http.MethodPost || reflect.TypeOf(route.Request) != reflect.TypeOf(DeliveryLandDecision{}) || reflect.TypeOf(route.Response) != reflect.TypeOf(DeliveryLandResponse{}) {
		t.Fatalf("Routes lists %+v for %s", route, path)
	}
	for _, c := range []struct {
		value  any
		fields []string
	}{
		{DeliveryPresentation{}, []string{"can_land", "recommendation"}},
		{DeliveryApproval{}, []string{"route"}},
		{InboxEntry{}, []string{"land"}},
		{DeliveryLandDecision{}, []string{"review", "review_revision", "commit", "draft_hash", "accept_blocking"}},
		{DeliveryLandResponse{}, []string{"approval", "outcome", "reason", "commit"}},
	} {
		tags := jsonFieldNames(reflect.TypeOf(c.value))
		for _, name := range c.fields {
			if !slices.Contains(tags, name) {
				t.Errorf("%T has no JSON field %q", c.value, name)
			}
		}
	}

	s, _ := start(t, fixture(t))
	target := strings.NewReplacer("{workstream}", string(fixturekit.Stream)).Replace(path)
	send := func(method, body string) ErrorResponse {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handle(rec, req)
		var failure ErrorResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &failure)
		return failure
	}
	all := `{"review":1,"review_revision":1,"commit":"c","draft_hash":"h","accept_blocking":true}`
	if got := send(http.MethodPost, all).Error.Code; got == Malformed || got == Unsupported {
		t.Errorf("a request holding every field of DeliveryLandDecision is %q", got)
	}
	if got := send(http.MethodPost, `{"review":1,"description":"x"}`).Error.Code; got != Malformed {
		t.Errorf("a request with an unknown field is %q, want %q", got, Malformed)
	}
	if got := send(http.MethodGet, "").Error.Code; got != Unsupported {
		t.Errorf("GET is %q, want %q", got, Unsupported)
	}
}

func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}
