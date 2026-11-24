package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
)

type reloadFixture struct {
	root     string
	path     string
	original []byte
	before   wire.ConfigResponse
}

func newReloadFixture(t *testing.T) reloadFixture {
	t.Helper()
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
	var before wire.ConfigResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "status", "--json")), &struct {
		Configuration *wire.ConfigResponse `json:"configuration"`
	}{&before}))
	return reloadFixture{root: root, path: path, original: original, before: before}
}

func (f reloadFixture) write(t *testing.T, extra string) {
	t.Helper()
	must(t, os.WriteFile(f.path, append(append([]byte{}, f.original...), extra...), 0600))
}

func TestReloadRefusesAnInvalidFileAndStatusNamesTheFailure(t *testing.T) {
	f := newReloadFixture(t)
	f.write(t, "[capacity]\nmasons = 0\n")
	code, out, diag := invoke(t, f.root, "reload")
	if code != 4 || out != "" || !strings.Contains(diag, f.path+": capacity.masons: must be positive") {
		t.Fatalf("invalid reload: %d %q %q", code, out, diag)
	}
	if status := successful(t, f.root, "status"); !strings.Contains(status, "Configuration: "+f.before.Digest) || !strings.Contains(status, "Last reload failed at ") || !strings.Contains(status, "capacity.masons") {
		t.Fatalf("status after a failed reload:\n%s", status)
	}
}

func TestReloadNamesTheSettingsThatNeedARestart(t *testing.T) {
	f := newReloadFixture(t)
	f.write(t, "[listen]\nsocket = \"local.sock\"\n[capacity]\nmasons = 7\n")
	var reloaded wire.ReloadResponse
	must(t, json.Unmarshal([]byte(successful(t, f.root, "reload", "--json")), &reloaded))
	if reloaded.Digest == f.before.Digest || len(reloaded.Digest) != 64 || len(reloaded.RestartRequired) != 1 || reloaded.RestartRequired[0] != "listen.socket" {
		t.Fatalf("reload: %+v", reloaded)
	}
	text := successful(t, f.root, "reload")
	if text != "Configuration reloaded: "+reloaded.Digest+"\nRestart required to apply: listen.socket\n" {
		t.Fatalf("reload text: %q", text)
	}
}

func TestStatusAfterAReloadShowsTheNewDigestAndNoFailure(t *testing.T) {
	f := newReloadFixture(t)
	f.write(t, "[capacity]\nmasons = 0\n")
	invoke(t, f.root, "reload")
	f.write(t, "[capacity]\nmasons = 7\n")
	var reloaded wire.ReloadResponse
	must(t, json.Unmarshal([]byte(successful(t, f.root, "reload", "--json")), &reloaded))
	if status := successful(t, f.root, "status"); !strings.Contains(status, "Configuration: "+reloaded.Digest) || strings.Contains(status, "Last reload failed") {
		t.Fatalf("status after a reload:\n%s", status)
	}
}

func TestReloadTakesNoArguments(t *testing.T) {
	f := newReloadFixture(t)
	if code, _, diag := invoke(t, f.root, "reload", "extra"); code != 2 || !strings.Contains(diag, "invalid arguments") {
		t.Fatalf("reload with arguments: %d %s", code, diag)
	}
}
