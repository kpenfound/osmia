package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
)

func TestProjectPreviewCommand(t *testing.T) {
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	var out api.ProjectPreviewResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "preview", project, "--call", "serve", "--call=--dev", "--port", "8080", "--json")), &out))
	if out.Preview == nil || strings.Join(out.Preview.Call, " ") != "serve --dev" || out.Preview.Path != "/" || out.Preview.Timeout != "5m" {
		t.Fatalf("defaults: %+v", out)
	}
	text := successful(t, root, "project", "preview", project, "--call", "serve", "--port", "8081", "--path", "/settings", "--timeout", "1m")
	if !strings.Contains(text, "port 8081, path /settings, timeout 1m") {
		t.Fatal(text)
	}
	for _, args := range [][]string{
		{"--call", "serve", "--port", "0"}, {"--port", "8080"},
		{"--call", "serve", "--port", "8080", "--path", "bad"},
		{"--call", "serve", "--port", "8080", "--timeout", "31m"},
	} {
		code, stdout, stderr := invoke(t, root, append([]string{"project", "preview", project}, args...)...)
		if code != 4 || stdout != "" || !strings.Contains(stderr, "validation:") || !strings.Contains(stderr, "preview.") {
			t.Fatalf("%v: %d %s %s", args, code, stdout, stderr)
		}
	}
	path := filepath.Join(root, "projects", project, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	hand := append(append([]byte{}, data...), []byte("# owner edit\n")...)
	must(t, os.WriteFile(path, hand, 0600))
	code, stdout, stderr := invoke(t, root, "project", "preview", project, "--clear")
	if code != 5 || stdout != "" || !strings.Contains(stderr, "conflict:") || !strings.Contains(stderr, "drift") {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
	successful(t, root, "reload")
	if text := successful(t, root, "project", "preview", project, "--clear"); !strings.Contains(text, "Previews disabled") {
		t.Fatal(text)
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "preview", project, "--clear", "--json")), &out))
	if out.Preview != nil {
		t.Fatal(out)
	}
}
