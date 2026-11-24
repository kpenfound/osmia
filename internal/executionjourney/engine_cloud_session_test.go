package executionjourney

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestServiceCloudSessionChecks(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	const token = "session-config-cloud-secret"
	const traceURL = "https://dagger.cloud/example/traces/service123"
	t.Setenv("OSMIA_ENGINE_TEST_TOKEN", token)
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "tcp://host-engine:1234")
	s, c, _, project, w, _, _ := newSessionControlsFixture(t, func(o *journey.Options) {
		path := filepath.Join(o.Config.Root, "config.toml")
		data, err := os.ReadFile(path)
		fixturekit.Must(t, err)
		data = append(data, []byte("\n[dagger]\nengine = 'cloud'\ncloud_compute_token_env = 'OSMIA_ENGINE_TEST_TOKEN'\n")...)
		fixturekit.Must(t, os.WriteFile(path, data, 0600))
	})

	service.InspectForTest(s.Core()).SetReviewChecks(nil)
	run, err := c.SessionCheck(t.Context(), w)
	fixturekit.Must(t, err)
	if run.Status != checks.ChecksPassed || run.TraceURL != traceURL || run.CIIdentityFallback != "" {
		t.Fatalf("session run: %+v", run)
	}
	repo, err := s.Repository(project)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](repo, w)
	fixturekit.Must(t, err)
	captured := ""
	for _, d := range docs {
		if d.Path == run.OutputPath {
			captured = d.Content
		}
		if strings.Contains(d.Content, token) {
			t.Fatalf("token in session trace %s", d.Path)
		}
	}
	for _, want := range []string{"ENGINE=cloud\n", "RUNNER=\n", "TOKEN_PRESENT=true\n", "CI=\n", "HEAD=" + run.Commit + "\n"} {
		if !strings.Contains(captured, want) {
			t.Fatalf("missing %q in %s", want, captured)
		}
	}
	var feed api.FeedResponse
	fixturekit.Must(t, c.Do(t.Context(), "GET", api.Prefix+"/feed/"+string(w), nil, &feed))
	found := false
	for _, e := range feed.Entries {
		if e.SessionChecks != nil && e.SessionChecks.TraceURL == traceURL {
			found = true
		}
	}
	data, _ := json.Marshal(feed)
	if !found || strings.Contains(string(data), token) {
		t.Fatalf("session feed: %s", data)
	}
	if fake.Commands(t) != "" {
		t.Fatalf("Cloud touched docker: %s", fake.Commands(t))
	}
}

func TestServiceDockerSessionChecks(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("DOCKER_HOST", "tcp://engine.example.ts.net:2375")
	t.Setenv("DOCKER_CONTEXT", "default")
	s, c, _, project, w, _, _ := newSessionControlsFixture(t, func(o *journey.Options) {
		path := filepath.Join(o.Config.Root, "config.toml")
		data, err := os.ReadFile(path)
		fixturekit.Must(t, err)
		data = append(data, []byte("\n[dagger]\nengine = 'managed'\nversion = 'v0.20.5'\n")...)
		fixturekit.Must(t, os.WriteFile(path, data, 0600))
	})
	service.InspectForTest(s.Core()).SetReviewChecks(nil)
	run, err := c.SessionCheck(t.Context(), w)
	fixturekit.Must(t, err)
	if run.Status != checks.ChecksPassed {
		t.Fatalf("%+v", run)
	}
	repo, err := s.Repository(project)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](repo, w)
	fixturekit.Must(t, err)
	captured := ""
	for _, d := range docs {
		if d.Path == run.OutputPath {
			captured = d.Content
		}
	}
	for _, want := range []string{"ENGINE=docker-container://osmia-dagger-engine\n", "RUNNER=docker-container://osmia-dagger-engine\n", "DOCKER_HOST=tcp://engine.example.ts.net:2375\n"} {
		if !strings.Contains(captured, want) {
			t.Fatalf("missing %q: %s", want, captured)
		}
	}
	log, err := os.ReadFile(fake.Environments)
	fixturekit.Must(t, err)
	if !strings.Contains(string(log), "dagger|check --progress=report --fail-fast --parallel 4|docker-container://osmia-dagger-engine|tcp://engine.example.ts.net:2375\n") {
		t.Fatalf("session check environment: %s", log)
	}
}
