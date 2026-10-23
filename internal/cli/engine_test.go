package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestEngineCommands(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	o := fixture(t)
	path := filepath.Join(o.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	managed := append(append([]byte{}, original...), []byte("\n[dagger]\nengine = 'managed'\nversion = 'v0.21.0'\n")...)
	must(t, os.WriteFile(path, managed, 0600))
	s, err := service.Start(t.Context(), o)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	text := successful(t, o.Config.Root, "engine", "status")
	for _, want := range []string{"mode=managed", "reachable=true", "v0.20.5", "configured=v0.21.0", "configured_mismatch=true", "4096 bytes"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	var status api.EngineStatus
	must(t, json.Unmarshal([]byte(successful(t, o.Config.Root, "engine", "status", "--json")), &status))
	if !status.Reachable || status.CacheSizeBytes == nil {
		t.Fatalf("JSON status: %+v", status)
	}
	release, err := service.InspectForTest(s).AcquireCheckSlot(t.Context(), service.CheckSlotRun{Project: fixturekit.Project, Workstream: fixturekit.Stream, Unit: "busy-unit"})
	must(t, err)
	for _, action := range []string{"restart", "prune"} {
		code, out, diag := invoke(t, o.Config.Root, "engine", action)
		if code != 3 || out != "" || !strings.Contains(diag, "temporarily unavailable") || !strings.Contains(diag, "busy-unit") {
			t.Fatalf("busy %s: %d %s %s", action, code, out, diag)
		}
	}
	release()
	for _, action := range []string{"restart", "prune"} {
		successful(t, o.Config.Root, "engine", action)
	}
	must(t, json.Unmarshal([]byte(successful(t, o.Config.Root, "engine", "status", "--json")), &status))
	if status.Version != "v0.21.0" || status.ConfiguredVersionMismatch {
		t.Fatalf("restarted status: %+v", status)
	}
	before := fake.Commands(t)
	must(t, os.WriteFile(path, original, 0600))
	successful(t, o.Config.Root, "reload")
	for _, action := range []string{"restart", "prune"} {
		code, _, diag := invoke(t, o.Config.Root, "engine", action)
		if code != 5 || !strings.Contains(diag, "not applicable") {
			t.Fatalf("unmanaged %s: %d %s", action, code, diag)
		}
	}
	if fake.Commands(t) != before {
		t.Fatal("auto controls touched Docker")
	}
	for _, args := range [][]string{{"engine"}, {"engine", "unknown"}, {"engine", "status", "extra"}} {
		if code, _, _ := invoke(t, o.Config.Root, args...); code != 2 {
			t.Fatalf("arguments accepted: %v", args)
		}
	}
}
