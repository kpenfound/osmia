package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const engineTestTrace = "https://dagger.cloud/example/traces/service123"
const engineTestToken = "service-cloud-test-secret"

func engineConfig(mode, token string) string {
	if mode == "" {
		return ""
	}
	return fmt.Sprintf("\n[dagger]\nengine = %q\nversion = 'v0.20.5'\ncloud_token_env = %q\n", mode, token)
}

// Serial: fake commands and planted host engine selectors are process-wide.
func TestServiceEngineUnitChecks(t *testing.T) {
	for _, tc := range []struct{ name, mode, token, runner string }{
		{"managed authenticated", "managed", "OSMIA_ENGINE_TEST_TOKEN", "docker-container://osmia-dagger-engine"},
		{"managed anonymous", "managed", "", "docker-container://osmia-dagger-engine"},
		{"legacy auto", "", "", "tcp://host-engine:1234"},
		{"explicit container", "docker-container://explicit", "", "docker-container://explicit"},
		{"explicit address", "tcp://configured-engine:1234", "OSMIA_ENGINE_TEST_TOKEN", "tcp://configured-engine:1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fixturekit.InstallEngineCLIs(t)
			t.Setenv("DOCKER_CONTEXT", "default")
			if tc.mode == "managed" {
				must(t, os.Remove(fake.Version))
			}
			var logs fixturekit.SyncBuffer
			t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "tcp://host-engine:1234")
			t.Setenv("DOCKER_HOST", "tcp://host-docker:2375")
			t.Setenv("OSMIA_ENGINE_TEST_TOKEN", engineTestToken)
			f, _ := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", independentPlan, "", func(o *Options) {
				appendEngineConfig(t, o, engineConfig(tc.mode, tc.token))
				o.reviewChecks = nil
				o.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			})
			base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
			w, repo := seedBuild(t, f, "engine-check", independentPlan, config.WorkspacesGit, base)
			seedChecking(t, f, repo, w, "resume", resumeReport)
			report := intakefixture.CurrentReport[UnitReport](t, repo, w, "resume", reportDocument)
			must(t, repo.Close())
			f.start(t)
			t.Cleanup(func() { f.stop(t) })
			var run UnitCheckRun
			var captured string
			deadline := time.Now().Add(20 * time.Second)
			for {
				r, err := f.s.repository(f.project)
				must(t, err)
				docs, err := trace.Read[trace.Document](r, w)
				must(t, err)
				for _, d := range docs {
					if d.Path == "units/resume/checks-1.json" {
						must(t, json.Unmarshal([]byte(d.Content), &run))
					}
					if strings.Contains(d.Content, "RUNNER=") {
						captured = d.Content
					}
					if strings.Contains(d.Content, engineTestToken) {
						t.Fatalf("token in trace %s", d.Path)
					}
				}
				if run.Status != "" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("unit check did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if run.Status != ChecksPassed || run.Candidate != report.Candidate || run.TraceURL != engineTestTrace || run.CIIdentityFallback != "" {
				t.Fatalf("unit run: %+v", run)
			}
			assertEngineCheckOutput(t, captured, tc.mode, tc.token, tc.runner, report.Candidate)
			if tc.mode == "managed" {
				env, err := os.ReadFile(fake.Environments)
				must(t, err)
				for _, want := range []string{"docker|container inspect osmia-dagger-engine||tcp://host-docker:2375", "registry.dagger.io/engine:v0.20.5||tcp://host-docker:2375", "dagger|check --progress=report --fail-fast --parallel 4|docker-container://osmia-dagger-engine|tcp://host-docker:2375"} {
					if !strings.Contains(string(env), want) {
						t.Fatalf("missing %q: %s", want, env)
					}
				}
			}
			feed := readFeed(t, f.c, w)
			found := false
			for _, e := range feed.Entries {
				if e.UnitChecks != nil && e.UnitChecks.TraceURL == engineTestTrace {
					found = true
				}
			}
			raw, _ := json.Marshal(feed)
			if !found || strings.Contains(string(raw), engineTestToken) {
				t.Fatalf("feed: %s", raw)
			}
			if strings.Contains(logs.String(), engineTestToken) {
				t.Fatal("Cloud token in service logs")
			}
			if tc.mode != "managed" && fake.Commands(t) != "" {
				t.Fatalf("external engine touched: %s", fake.Commands(t))
			}
		})
	}
}

func assertEngineCheckOutput(t *testing.T, output, mode, token, runner, candidate string) {
	t.Helper()
	for _, want := range []string{"RUNNER=" + runner + "\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in %s", want, output)
		}
	}
	if token != "" {
		for _, want := range []string{"CI=\n", "TOKEN_PRESENT=true\n", "HEAD=" + candidate + "\n"} {
			if !strings.Contains(output, want) {
				t.Fatalf("missing %q in %s", want, output)
			}
		}
	} else {
		for _, want := range []string{"CI=\n", "TOKEN_PRESENT=\n", "ANONYMOUS_EXPORT\n"} {
			if !strings.Contains(output, want) {
				t.Fatalf("missing %q in %s", want, output)
			}
		}
	}
	if (mode == "" || mode == "managed" || strings.HasPrefix(mode, "docker-container://")) && os.Getenv("DOCKER_HOST") != "" && !strings.Contains(output, "DOCKER_HOST="+os.Getenv("DOCKER_HOST")+"\n") {
		t.Fatalf("legacy host environment: %s", output)
	}
	if strings.Contains(output, engineTestToken) {
		t.Fatal("token in captured output")
	}
}

func TestServiceCloudFinalChecks(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("OSMIA_ENGINE_TEST_TOKEN", engineTestToken)
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "tcp://host-engine:1234")
	f, _ := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", validPlan, "", func(o *Options) {
		appendEngineConfig(t, o, engineConfig("cloud", "OSMIA_ENGINE_TEST_TOKEN"))
		o.reviewChecks = nil
	})
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	w, repo := seedBuild(t, f, "cloud-final", validPlan, config.WorkspacesGit, base)
	defer repo.Close()
	candidate := moveFeature(t, f, w, map[string]string{"candidate.txt": "cloud candidate\n"})
	a := &delivery.FinalReviewer{S: f.s.delivery(), Repository: repo}
	run, err := a.FinalChecks(t.Context(), f.s.current(), w, FinalReport{Review: 1, Operation: "cloud-final", Commit: candidate, Upstream: &seal.Base{Commit: base}})
	must(t, err)
	if run.Status != ChecksPassed || run.TraceURL != engineTestTrace {
		t.Fatalf("final run: %+v", run)
	}
	docs, err := trace.Read[trace.Document](repo, w)
	must(t, err)
	output := ""
	for _, d := range docs {
		if strings.Contains(d.Content, "ENGINE=cloud") {
			output = d.Content
		}
		if strings.Contains(d.Content, engineTestToken) {
			t.Fatal("token in final trace")
		}
	}
	assertEngineCheckOutput(t, output, "cloud", "OSMIA_ENGINE_TEST_TOKEN", "", candidate)
	feed, err := feedChecks(repo, w)
	must(t, err)
	found := false
	for _, e := range feed {
		if e.FinalChecks != nil && e.FinalChecks.TraceURL == engineTestTrace {
			found = true
		}
	}
	if !found {
		t.Fatalf("final feed: %+v", feed)
	}
	_, settings, err := f.s.current().Execution("mason", "default")
	must(t, err)
	data, _ := json.Marshal(settings)
	if settings.Dagger != nil || strings.Contains(string(data), engineTestToken) || strings.Contains(string(data), "cloud") {
		t.Fatalf("mason settings: %s", data)
	}
	if fake.Commands(t) != "" {
		t.Fatalf("Cloud touched docker: %s", fake.Commands(t))
	}
}

func appendEngineConfig(t *testing.T, o *Options, text string) {
	t.Helper()
	path := filepath.Join(o.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(text)...), 0600))
}

func TestServiceEngineReloadAndControls(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("OSMIA_ENGINE_TEST_TOKEN", engineTestToken)
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "tcp://host-engine:1234")
	o := fixture(t)
	files := configFiles(t, o)
	top := files.topText + engineConfig("managed", "") + "\n[roles.mason]\nsandbox = 'sbx'\nimage = 'fixture-image'\n[roles.mason.dagger]\nversion = 'v0.20.5'\n"
	files.write(t, top, files.projectText)
	must(t, os.Remove(fake.Version))
	s, c := start(t, o)
	if !strings.Contains(fake.Commands(t), "registry.dagger.io/engine:v0.20.5") {
		t.Fatalf("startup did not create managed engine: %s", fake.Commands(t))
	}
	must(t, os.WriteFile(fake.DockerCommands, nil, 0600))
	settings := threadExecution(masonRole, s.current().Roles[masonRole])
	if settings.Dagger == nil || settings.Dagger.Engine != "docker-container://osmia-dagger-engine" {
		t.Fatalf("mason default: %+v", settings)
	}
	data, _ := json.Marshal(settings)
	if strings.Contains(string(data), engineTestToken) || strings.Contains(string(data), "cloud") {
		t.Fatalf("mason credential settings: %s", data)
	}
	held, err := s.checkSlots.acquire(t.Context(), CheckSlotRun{Project: project, Workstream: stream, Unit: "checking-unit"})
	must(t, err)
	turn, err := s.checkSlots.startEngineTurn(t.Context(), EngineTurn{Project: project, Workstream: stream, Unit: "working-unit", Agent: "mason-working", Turn: "turn-1"})
	must(t, err)
	defer held()
	defer turn()
	next := strings.Replace(top, "version = 'v0.20.5'", "version = 'v0.21.0'", 1)
	files.write(t, next, files.projectText)
	_, err = c.Reload(t.Context())
	must(t, err)
	for _, action := range []string{"restart", "prune"} {
		_, err := c.EngineAction(t.Context(), action)
		var api *APIError
		if !errors.As(err, &api) || api.Code != Unavailable || !strings.Contains(api.Message, "checking-unit") || !strings.Contains(api.Message, "mason-working") {
			t.Fatalf("busy %s: %v", action, err)
		}
	}
	commands := fake.Commands(t)
	for _, line := range strings.Split(commands, "\n") {
		if !strings.HasPrefix(line, "container inspect ") {
			t.Fatalf("reload touched running engine: %s", commands)
		}
	}
	status, err := c.EngineStatus(t.Context())
	must(t, err)
	if status.Mode != config.DaggerEngineManaged || !status.Reachable || status.Version != "v0.20.5" || status.ExpectedVersion != "v0.21.0" || !status.ConfiguredVersionMismatch || status.CacheSizeBytes == nil || *status.CacheSizeBytes != 4096 {
		t.Fatalf("reloaded status: %+v", status)
	}
	// Invalid reload leaves the engine selection and the check client bound to managed.
	files.write(t, strings.Replace(next, "engine = \"managed\"", "engine = 'bad-mode'", 1), files.projectText)
	if _, err = c.Reload(t.Context()); err == nil {
		t.Fatal("invalid reload accepted")
	}
	result, err := s.configuredChecks(s.current()).Check(t.Context(), t.TempDir(), checks.Identity{}, nil)
	must(t, err)
	assertEngineCheckOutput(t, result.Output, "managed", "", "docker-container://osmia-dagger-engine", "")
	held()
	turn()
	_, err = c.EngineAction(t.Context(), "restart")
	must(t, err)
	status, err = c.EngineStatus(t.Context())
	must(t, err)
	if status.Version != "v0.21.0" || status.ConfiguredVersionMismatch || !status.VersionMismatch {
		t.Fatalf("restarted status: %+v", status)
	}
	_, err = c.EngineAction(t.Context(), "prune")
	must(t, err)
	commands = fake.Commands(t)
	for _, want := range []string{"stop osmia-dagger-engine", "rm osmia-dagger-engine", "osmia-dagger-engine-cache:/var/lib/dagger registry.dagger.io/engine:v0.21.0"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("restart missing %q: %s", want, commands)
		}
	}
	for _, mode := range []string{"auto", "tcp://external:1234", "cloud"} {
		files.write(t, files.topText+engineConfig(mode, "OSMIA_ENGINE_TEST_TOKEN"), files.projectText)
		_, err = c.Reload(t.Context())
		must(t, err)
		before := fake.Commands(t)
		for _, action := range []string{"restart", "prune"} {
			_, err = c.EngineAction(t.Context(), action)
			var api *APIError
			if !errors.As(err, &api) || api.Code != Conflict || !strings.Contains(api.Message, "not applicable") {
				t.Fatalf("mode %s %s: %v", mode, action, err)
			}
		}
		status, err = c.EngineStatus(t.Context())
		must(t, err)
		if !status.Reachable || status.Version == "" || status.CacheSizeBytes == nil {
			t.Fatalf("%s status: %+v", mode, status)
		}
		if fake.Commands(t) != before {
			t.Fatalf("%s touched docker", mode)
		}
	}
}

// Serial: startup, reload and commands share process Docker selectors.
func TestServiceDockerSelectorConflict(t *testing.T) {
	for _, mode := range []string{"managed", "docker-container://explicit"} {
		t.Run(mode, func(t *testing.T) {
			fake := fixturekit.InstallEngineCLIs(t)
			t.Setenv("DOCKER_HOST", "ssh://user:secret@engine.example.ts.net")
			t.Setenv("DOCKER_CONTEXT", "named")
			o := fixture(t)
			files := configFiles(t, o)
			files.write(t, files.topText+engineConfig(mode, ""), files.projectText)
			s, err := Start(t.Context(), o)
			assertConflict := func(err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") || !strings.Contains(err.Error(), "DOCKER_CONTEXT") {
					t.Fatalf("%v", err)
				}
			}
			assertConflict(err)
			if s != nil {
				_ = s.Close()
				t.Fatal("conflicting engine started")
			}
			if _, err := os.Stat(fake.Environments); !os.IsNotExist(err) {
				t.Fatalf("startup invoked engine: %v", err)
			}
			files.write(t, files.topText, files.projectText)
			s, c := start(t, o)
			before := s.current()
			files.write(t, files.topText+engineConfig(mode, ""), files.projectText)
			_, err = c.Reload(t.Context())
			assertConflict(err)
			if s.current() != before {
				t.Fatal("failed reload replaced loaded configuration")
			}
			// Install a valid selection, then model the conflicting process environment
			// to exercise status and action routes independently of startup refusal.
			t.Setenv("DOCKER_CONTEXT", "default")
			_, err = c.Reload(t.Context())
			must(t, err)
			t.Setenv("DOCKER_CONTEXT", "named")
			must(t, os.WriteFile(fake.Environments, nil, 0600))
			status, err := c.EngineStatus(t.Context())
			must(t, err)
			if status.Reachable || status.DockerHost != "" || !strings.Contains(status.ReachabilityError, "DOCKER_HOST") || !strings.Contains(status.ReachabilityError, "DOCKER_CONTEXT") {
				t.Fatalf("%+v", status)
			}
			for _, action := range []string{"restart", "prune"} {
				_, err = c.EngineAction(t.Context(), action)
				assertConflict(err)
			}
			_, err = s.configuredChecks(s.current()).Check(t.Context(), t.TempDir(), checks.Identity{}, nil)
			assertConflict(err)
			log, err := os.ReadFile(fake.Environments)
			must(t, err)
			if len(log) != 0 {
				t.Fatalf("conflict reached engine: %s", log)
			}
		})
	}
}

// Serial: final-review checks use fake CLIs in the process environment.
func TestServiceDockerFinalChecks(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("DOCKER_HOST", "ssh://engine.example.ts.net")
	t.Setenv("DOCKER_CONTEXT", "default")
	f, _ := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", validPlan, "", func(o *Options) {
		appendEngineConfig(t, o, engineConfig("managed", ""))
		o.reviewChecks = nil
	})
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	w, repo := seedBuild(t, f, "docker-final", validPlan, config.WorkspacesGit, base)
	defer repo.Close()
	candidate := moveFeature(t, f, w, map[string]string{"candidate.txt": "remote engine candidate\n"})
	a := &delivery.FinalReviewer{S: f.s.delivery(), Repository: repo}
	run, err := a.FinalChecks(t.Context(), f.s.current(), w, FinalReport{Review: 1, Operation: "docker-final", Commit: candidate, Upstream: &seal.Base{Commit: base}})
	must(t, err)
	if run.Status != ChecksPassed {
		t.Fatalf("%+v", run)
	}
	log, err := os.ReadFile(fake.Environments)
	must(t, err)
	if !strings.Contains(string(log), "dagger|check --progress=report --fail-fast --parallel 4|docker-container://osmia-dagger-engine|ssh://engine.example.ts.net\n") {
		t.Fatalf("final check environment: %s", log)
	}
}

// Serial: the API invokes fake Docker through the process PATH.
func TestCreatingEngineAPIRespondsAtDeadline(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("DOCKER_HOST", "tcp://engine.example.ts.net:2375")
	t.Setenv("DOCKER_CONTEXT", "default")
	o := fixture(t)
	files := configFiles(t, o)
	files.write(t, files.topText+engineConfig("managed", ""), files.projectText)
	s, _ := start(t, o)
	entered := filepath.Join(fake.Directory, "creating")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
 container) sleep 0.03; echo '[{"State":{"Running":true}}]' ;;
 stop|rm) sleep 0.03 ;;
 run) touch %q; exec sleep 10 ;;
 *) exit 92 ;;
esac
`, entered)
	must(t, os.WriteFile(filepath.Join(fake.Directory, "docker"), []byte(script), 0700))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancel()
		s.engineHTTP(w, r.WithContext(ctx))
	}))
	defer server.Close()
	completed := make(chan error, 1)
	started := time.Now()
	go func() {
		response, err := http.Post(server.URL+Prefix+"/engine/restart", "application/json", nil)
		if err == nil {
			defer response.Body.Close()
			data, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				err = readErr
			} else if response.StatusCode != 500 || !strings.Contains(string(data), "deadline exceeded") {
				err = fmt.Errorf("response %d: %s", response.StatusCode, data)
			}
		}
		completed <- err
	}()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case err := <-completed:
			t.Fatalf("ended before creation: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	next := queuedSlot(t.Context(), s.checkSlots, CheckSlotRun{Unit: "waiting-for-creation"})
	must(t, awaitCheck(t, completed))
	if time.Since(started) > 2*time.Second {
		t.Fatal("API did not deliver response at the action bound")
	}
	release := awaitCheck(t, next)
	if release == nil {
		t.Fatal("API action retained maintenance gate")
	}
	release()
}
