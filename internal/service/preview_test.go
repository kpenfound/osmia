package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestProjectPreviewSettingsAreUnavailable(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	files := configFiles(t, opts)
	text := files.projectText + "# kept verbatim\n[preview]\ncall = ['unused']\nport = 1\npath = '/unused'\ntimeout = 'invalid'\n"
	files.write(t, files.topText, text)
	s, c := start(t, opts)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		unknown := httptest.NewRecorder()
		s.handle(unknown, httptest.NewRequest(method, Prefix+"/unknown", nil))
		rec := httptest.NewRecorder()
		s.handle(rec, httptest.NewRequest(method, Prefix+"/projects/preview/"+string(project), strings.NewReader(`{"call":["serve"],"port":8080}`)))
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Fatalf("%s: %d %s; unknown: %d %s", method, rec.Code, rec.Body, unknown.Code, unknown.Body)
		}
	}
	for _, route := range Routes {
		if strings.Contains(route.Path, "/projects/preview/") {
			t.Fatal(route)
		}
	}
	for i := 0; i < 2; i++ {
		cfg, err := c.Configuration(context.Background())
		must(t, err)
		data, err := json.Marshal(cfg.Effective)
		must(t, err)
		if strings.Contains(string(data), `"preview"`) {
			t.Fatalf("effective config exposes preview: %s", data)
		}
		data, err = os.ReadFile(files.project)
		must(t, err)
		if string(data) != text {
			t.Fatal("project file rewritten")
		}
		_, err = c.Reload(context.Background())
		must(t, err)
	}
}
