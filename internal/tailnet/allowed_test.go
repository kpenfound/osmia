package tailnet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

func allowed(method, path, host, contentType string) bool {
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return Allowed(r, "osmia", func(context.Context) []string { return []string{"osmia-1.tail0.ts.net."} })
}

func TestAllowedKeepsTheJSONRuleForEveryRouteButTwo(t *testing.T) {
	t.Parallel()
	project := api.GitPath("p_0123456789abcdef0123456789abcdef")
	for _, c := range []struct {
		name                        string
		method, path, host, content string
		want                        bool
	}{
		{"a read", "GET", "/v1/status", "osmia", "", true},
		{"a JSON write", "PUT", "/v1/runtime/pause", "osmia", "application/json; charset=utf-8", true},
		{"a write without a type", "PUT", "/v1/runtime/pause", "osmia", "", false},
		{"a form write", "POST", "/v1/reload", "osmia", "application/x-www-form-urlencoded", false},
		{"git fetch with its own type", "POST", project + "/git-upload-pack", "osmia", api.GitUploadPackRequestType, true},
		{"git fetch by IP address", "POST", project + "/git-upload-pack", "100.101.102.103", api.GitUploadPackRequestType, true},
		{"git fetch by the node's name", "POST", project + "/git-upload-pack", "osmia-1.tail0.ts.net", api.GitUploadPackRequestType + "; x=y", true},
		{"git fetch with JSON", "POST", project + "/git-upload-pack", "osmia", "application/json", true},
		{"git fetch with another host", "POST", project + "/git-upload-pack", "attacker.example", api.GitUploadPackRequestType, false},
		{"git fetch with the upload type", "POST", project + "/git-upload-pack", "osmia", api.ViewArchiveType, false},
		{"git fetch without a type", "POST", project + "/git-upload-pack", "osmia", "", false},
		{"git fetch with a form type", "POST", project + "/git-upload-pack", "osmia", "text/plain", false},
		{"git receive-pack with the fetch type", "POST", project + "/git-receive-pack", "osmia", api.GitUploadPackRequestType, false},
		{"git receive-pack with its own type", "POST", project + "/git-receive-pack", "osmia", "application/x-git-receive-pack-request", false},
		{"the fetch type on another route", "POST", "/v1/reload", "osmia", api.GitUploadPackRequestType, false},
		{"the fetch type on a PUT", "PUT", project + "/git-upload-pack", "osmia", api.GitUploadPackRequestType, false},
		{"the fetch type below the route", "POST", project + "/git-upload-pack/x", "osmia", api.GitUploadPackRequestType, false},
		{"a preflight to git fetch", "OPTIONS", project + "/git-upload-pack", "osmia", "", false},
		{"upload with its own type", "PUT", api.ViewPath("turn-1"), "osmia", api.ViewArchiveType, true},
		{"upload with another host", "PUT", api.ViewPath("turn-1"), "osmia.attacker.example", api.ViewArchiveType, false},
		{"upload with the fetch type", "PUT", api.ViewPath("turn-1"), "osmia", api.GitUploadPackRequestType, false},
		{"upload without a type", "PUT", api.ViewPath("turn-1"), "osmia", "", false},
		{"upload by POST", "POST", api.ViewPath("turn-1"), "osmia", api.ViewArchiveType, false},
		{"upload by DELETE", "DELETE", api.ViewPath("turn-1"), "osmia", api.ViewArchiveType, false},
		{"the upload type on another route", "PUT", "/v1/runtime/pause", "osmia", api.ViewArchiveType, false},
		{"the upload type below the route", "PUT", api.ViewPath("turn-1") + "/x", "osmia", api.ViewArchiveType, false},
		{"a preflight to upload", "OPTIONS", api.ViewPath("turn-1"), "osmia", "", false},
		{"a download", "GET", api.ViewPath("turn-1"), "osmia", "", true},
		{"a download from another host", "GET", api.ViewPath("turn-1"), "attacker.example", "", false},
	} {
		if got := allowed(c.method, c.path, c.host, c.content); got != c.want {
			t.Errorf("%s: allowed %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWebListenerKeepsTheJSONRuleOnTheFileRoutes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ method, path, content string }{
		{http.MethodPost, api.GitPath("p_0123456789abcdef0123456789abcdef") + "/git-upload-pack", api.GitUploadPackRequestType},
		{http.MethodPut, api.ViewPath("turn-1"), api.ViewArchiveType},
	} {
		r := httptest.NewRequest(c.method, "http://localhost"+c.path, nil)
		r.Header.Set("Content-Type", c.content)
		if WebAllowed(r) {
			t.Errorf("%s %s with %s is allowed on the web listener", c.method, c.path, c.content)
		}
	}
}
