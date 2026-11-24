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

type engineRoot struct {
	fake     fixturekit.EngineCLIs
	root     string
	service  *service.Service
	path     string
	original []byte
}

const managedEngineConfig = "\n[dagger]\nengine = 'managed'\nversion = 'v0.21.0'\n"

func startEngineRoot(t *testing.T, daggerConfig string) engineRoot {
	t.Helper()
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("DOCKER_HOST", "ssh://user:private@engine.example.ts.net")
	t.Setenv("DOCKER_CONTEXT", "default")
	o := fixture(t)
	path := filepath.Join(o.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	e := engineRoot{fake: fake, root: o.Config.Root, path: path, original: original}
	must(t, os.WriteFile(path, append(append([]byte{}, original...), []byte(daggerConfig)...), 0600))
	e.service, err = service.Start(t.Context(), o)
	must(t, err)
	t.Cleanup(func() { e.service.Close() })
	return e
}

func (e engineRoot) rewrite(t *testing.T, daggerConfig string) {
	t.Helper()
	must(t, os.WriteFile(e.path, append(append([]byte{}, e.original...), []byte(daggerConfig)...), 0600))
	successful(t, e.root, "reload")
}

func TestEngineStatusReportsTheManagedEngine(t *testing.T) {
	e := startEngineRoot(t, managedEngineConfig)
	text := successful(t, e.root, "engine", "status")
	for _, want := range []string{"mode=managed", "reachable=true", "v0.20.5", "configured=v0.21.0", "configured_mismatch=true", "4096 bytes", "Docker host: ssh://engine.example.ts.net"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
}

func TestEngineStatusOmitsDockerHostCredentials(t *testing.T) {
	e := startEngineRoot(t, managedEngineConfig)
	text := successful(t, e.root, "engine", "status")
	if strings.Contains(text, "user") || strings.Contains(text, "private") {
		t.Fatalf("host credentials: %s", text)
	}
}

func TestEngineStatusJSON(t *testing.T) {
	e := startEngineRoot(t, managedEngineConfig)
	var status api.EngineStatus
	must(t, json.Unmarshal([]byte(successful(t, e.root, "engine", "status", "--json")), &status))
	if !status.Reachable || status.CacheSizeBytes == nil || status.DockerHost != "ssh://engine.example.ts.net" {
		t.Fatalf("JSON status: %+v", status)
	}
}

func TestEngineRestartAndPruneRefuseWhileACheckSlotIsHeld(t *testing.T) {
	e := startEngineRoot(t, managedEngineConfig)
	release, err := service.InspectForTest(e.service).AcquireCheckSlot(t.Context(), service.CheckSlotRun{Project: fixturekit.Project, Workstream: fixturekit.Stream, Unit: "busy-unit"})
	must(t, err)
	defer release()
	for _, action := range []string{"restart", "prune"} {
		code, out, diag := invoke(t, e.root, "engine", action)
		if code != 3 || out != "" || !strings.Contains(diag, "temporarily unavailable") || !strings.Contains(diag, "busy-unit") {
			t.Fatalf("busy %s: %d %s %s", action, code, out, diag)
		}
	}
}

func TestEngineRestartAdoptsTheConfiguredVersion(t *testing.T) {
	e := startEngineRoot(t, managedEngineConfig)
	for _, action := range []string{"restart", "prune"} {
		successful(t, e.root, "engine", action)
	}
	var status api.EngineStatus
	must(t, json.Unmarshal([]byte(successful(t, e.root, "engine", "status", "--json")), &status))
	if status.Version != "v0.21.0" || status.ConfiguredVersionMismatch {
		t.Fatalf("restarted status: %+v", status)
	}
}

func TestEngineStatusNamesTheDockerHostOfAnExternalEngine(t *testing.T) {
	e := startEngineRoot(t, managedEngineConfig)
	e.rewrite(t, "\n[dagger]\nengine = 'docker-container://explicit'\n")
	text := successful(t, e.root, "engine", "status")
	if !strings.Contains(text, "Docker host: ssh://engine.example.ts.net") {
		t.Fatal(text)
	}
}

func TestEngineStatusReportsConflictingDockerSelectors(t *testing.T) {
	e := startEngineRoot(t, "\n[dagger]\nengine = 'docker-container://explicit'\n")
	t.Setenv("DOCKER_CONTEXT", "named")
	text := successful(t, e.root, "engine", "status")
	if !strings.Contains(text, "DOCKER_HOST") || !strings.Contains(text, "DOCKER_CONTEXT") || strings.Contains(text, "Docker host:") {
		t.Fatalf("conflicting selectors: %s", text)
	}
}

func TestEngineRestartAndPruneAreNotApplicableToAnAutomaticEngine(t *testing.T) {
	e := startEngineRoot(t, "")
	before := e.fake.Commands(t)
	for _, action := range []string{"restart", "prune"} {
		code, _, diag := invoke(t, e.root, "engine", action)
		if code != 5 || !strings.Contains(diag, "not applicable") {
			t.Fatalf("unmanaged %s: %d %s", action, code, diag)
		}
	}
	if e.fake.Commands(t) != before {
		t.Fatal("auto controls touched Docker")
	}
}

func TestEngineRefusesInvalidArguments(t *testing.T) {
	e := startEngineRoot(t, "")
	for _, args := range [][]string{{"engine"}, {"engine", "unknown"}, {"engine", "status", "extra"}} {
		if code, _, _ := invoke(t, e.root, args...); code != 2 {
			t.Fatalf("arguments accepted: %v", args)
		}
	}
}
