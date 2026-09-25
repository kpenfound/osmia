package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/service"
)

func TestReloadCommand(t *testing.T) {
	opts := fixture(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	resolved, err := filepath.EvalSymlinks(root)
	must(t, err)
	path := filepath.Join(resolved, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	var before service.ConfigResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &struct {
		Configuration *service.ConfigResponse `json:"configuration"`
	}{&before}))

	must(t, os.WriteFile(path, append(append([]byte{}, original...), "[capacity]\nmasons = 0\n"...), 0600))
	code, out, diag := invoke(t, root, "reload")
	if code != 4 || out != "" || !strings.Contains(diag, path+": capacity.masons: must be positive") {
		t.Fatalf("invalid reload: %d %q %q", code, out, diag)
	}
	if status := successful(t, root, "status"); !strings.Contains(status, "Configuration: "+before.Digest) || !strings.Contains(status, "Last reload failed at ") || !strings.Contains(status, "capacity.masons") {
		t.Fatalf("status after a failed reload:\n%s", status)
	}

	must(t, os.WriteFile(path, append(append([]byte{}, original...), "[listen]\nsocket = \"local.sock\"\n[capacity]\nmasons = 7\n"...), 0600))
	var reloaded service.ReloadResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "reload", "--json")), &reloaded))
	if reloaded.Digest == before.Digest || len(reloaded.Digest) != 64 || len(reloaded.RestartRequired) != 1 || reloaded.RestartRequired[0] != "listen.socket" {
		t.Fatalf("reload: %+v", reloaded)
	}
	text := successful(t, root, "reload")
	if text != "Configuration reloaded: "+reloaded.Digest+"\nRestart required to apply: listen.socket\n" {
		t.Fatalf("reload text: %q", text)
	}
	if status := successful(t, root, "status"); !strings.Contains(status, "Configuration: "+reloaded.Digest) || strings.Contains(status, "Last reload failed") {
		t.Fatalf("status after a reload:\n%s", status)
	}
	if code, _, diag := invoke(t, root, "reload", "extra"); code != 2 || !strings.Contains(diag, "invalid arguments") {
		t.Fatalf("reload with arguments: %d %s", code, diag)
	}
}
