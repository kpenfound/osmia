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
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
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
	return fmt.Sprintf("\n[dagger]\nengine = %q\nversion = 'v0.20.5'\ncloud_compute_token_env = %q\n", mode, token)
}

type engineUnitCase struct{ name, mode, token, runner string }

var (
	managedAuthenticated = engineUnitCase{"managed authenticated", "managed", "OSMIA_ENGINE_TEST_TOKEN", "docker-container://osmia-dagger-engine"}
	managedAnonymous     = engineUnitCase{"managed anonymous", "managed", "", "docker-container://osmia-dagger-engine"}
	hostEngine           = engineUnitCase{"legacy auto", "", "", "tcp://host-engine:1234"}
	explicitContainer    = engineUnitCase{"explicit container", "docker-container://explicit", "", "docker-container://explicit"}
	explicitAddress      = engineUnitCase{"explicit address", "tcp://configured-engine:1234", "OSMIA_ENGINE_TEST_TOKEN", "tcp://configured-engine:1234"}
)

type engineUnitRun struct {
	fake      fixturekit.EngineCLIs
	f         *shedFixture
	stream    config.WorkstreamID
	run       UnitCheckRun
	captured  string
	candidate string
	logs      *fixturekit.SyncBuffer
}

// runEngineUnitCheck runs the checks of a unit on the engine tc configures and
// returns the record of the run and the output the check captured. It fails
// when a trace document carries the Cloud token.
func runEngineUnitCheck(t *testing.T, tc engineUnitCase) engineUnitRun {
	t.Helper()
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("DOCKER_CONTEXT", "default")
	if tc.mode == "managed" {
		intakefixture.Must(t, os.Remove(fake.Version))
	}
	logs := &fixturekit.SyncBuffer{}
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "tcp://host-engine:1234")
	t.Setenv("DOCKER_HOST", "tcp://host-docker:2375")
	t.Setenv("OSMIA_ENGINE_TEST_TOKEN", engineTestToken)
	f, _ := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", independentPlan, func(o *Options) {
		appendEngineConfig(t, o, engineConfig(tc.mode, tc.token))
		o.reviewChecks = nil
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	w, repo := seedBuild(t, f, "engine-check", independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repo, w, "resume", resumeReport)
	report := intakefixture.CurrentReport[UnitReport](t, repo, w, "resume", reportDocument)
	intakefixture.Must(t, repo.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	out := engineUnitRun{fake: fake, f: f, stream: w, candidate: report.Candidate, logs: logs}
	intakefixture.Soon(t, "the unit check to finish", func() bool {
		r, err := f.s.repository(f.project)
		intakefixture.Must(t, err)
		docs, err := trace.Read[trace.Document](r, w)
		intakefixture.Must(t, err)
		for _, d := range docs {
			if d.Path == "units/resume/checks-1.json" {
				intakefixture.Must(t, json.Unmarshal([]byte(d.Content), &out.run))
			}
			if strings.Contains(d.Content, "RUNNER=") {
				out.captured = d.Content
			}
			if strings.Contains(d.Content, engineTestToken) {
				t.Fatalf("token in trace %s", d.Path)
			}
		}
		return out.run.Status != ""
	})
	return out
}

func eachEngineUnitCase(t *testing.T, cases []engineUnitCase, check func(t *testing.T, tc engineUnitCase, r engineUnitRun)) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { check(t, tc, runEngineUnitCheck(t, tc)) })
	}
}

// Serial: fake commands and planted host engine selectors are process-wide.
func TestUnitChecksPassOnTheConfiguredEngineForTheReportedCandidate(t *testing.T) {
	eachEngineUnitCase(t, []engineUnitCase{managedAuthenticated, managedAnonymous, hostEngine, explicitContainer, explicitAddress}, func(t *testing.T, tc engineUnitCase, r engineUnitRun) {
		if r.run.Status != ChecksPassed || r.run.Candidate != r.candidate || r.run.CIIdentityFallback != "" {
			t.Fatalf("unit run: %+v", r.run)
		}
		assertEngineCheckOutput(t, r.captured, tc.mode, tc.token, tc.runner, r.candidate)
	})
}

// Serial: fake commands and planted host engine selectors are process-wide.
func TestUnitCheckRunLinksItsCloudTraceOnlyWhenACloudTokenIsConfigured(t *testing.T) {
	eachEngineUnitCase(t, []engineUnitCase{managedAuthenticated, managedAnonymous}, func(t *testing.T, tc engineUnitCase, r engineUnitRun) {
		want := ""
		if tc.token != "" {
			want = engineTestTrace
		}
		if r.run.TraceURL != want {
			t.Fatalf("trace URL %q, want %q", r.run.TraceURL, want)
		}
		found, linked := false, false
		for _, e := range readFeed(t, r.f.c, r.stream).Entries {
			if e.UnitChecks == nil {
				continue
			}
			found = found || e.UnitChecks.TraceURL == engineTestTrace
			linked = linked || e.UnitChecks.TraceURL != ""
		}
		if tc.token != "" && !found || tc.token == "" && linked {
			t.Fatalf("feed links the trace: found %v, linked %v", found, linked)
		}
	})
}

// Serial: fake commands and planted host engine selectors are process-wide.
func TestUnitChecksKeepTheCloudTokenOutOfTheFeedAndTheLogs(t *testing.T) {
	eachEngineUnitCase(t, []engineUnitCase{managedAuthenticated, managedAnonymous, hostEngine, explicitContainer, explicitAddress}, func(t *testing.T, _ engineUnitCase, r engineUnitRun) {
		raw, _ := json.Marshal(readFeed(t, r.f.c, r.stream))
		if strings.Contains(string(raw), engineTestToken) {
			t.Fatalf("feed: %s", raw)
		}
		if strings.Contains(r.logs.String(), engineTestToken) {
			t.Fatal("Cloud token in service logs")
		}
	})
}

// Serial: fake commands and planted host engine selectors are process-wide.
func TestManagedEngineUnitChecksRunInTheManagedContainer(t *testing.T) {
	eachEngineUnitCase(t, []engineUnitCase{managedAuthenticated, managedAnonymous}, func(t *testing.T, _ engineUnitCase, r engineUnitRun) {
		env, err := os.ReadFile(r.fake.Environments)
		intakefixture.Must(t, err)
		for _, want := range []string{"docker|container inspect osmia-dagger-engine||tcp://host-docker:2375", "registry.dagger.io/engine:v0.20.5||tcp://host-docker:2375", "dagger|check --progress=report --fail-fast --parallel 4|docker-container://osmia-dagger-engine|tcp://host-docker:2375"} {
			if !strings.Contains(string(env), want) {
				t.Fatalf("missing %q: %s", want, env)
			}
		}
	})
}

// Serial: fake commands and planted host engine selectors are process-wide.
func TestUnitChecksNeverTouchAnExternalEngine(t *testing.T) {
	eachEngineUnitCase(t, []engineUnitCase{hostEngine, explicitContainer, explicitAddress}, func(t *testing.T, _ engineUnitCase, r engineUnitRun) {
		if got := r.fake.Commands(t); got != "" {
			t.Fatalf("external engine touched: %s", got)
		}
	})
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

func appendEngineConfig(t *testing.T, o *Options, text string) {
	t.Helper()
	path := filepath.Join(o.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(path, append(data, []byte(text)...), 0600))
}

type engineReload struct {
	fake  fixturekit.EngineCLIs
	files reloadFiles
	top   string
	next  string
	s     *Service
	c     *Client
	// releaseWork ends the unit check and the engine turn that keep the engine busy.
	releaseWork func()
}

// startManagedEngine starts a service whose managed engine is created at
// v0.20.5 with a sandboxed mason role.
func startManagedEngine(t *testing.T) *engineReload {
	t.Helper()
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("OSMIA_ENGINE_TEST_TOKEN", engineTestToken)
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "tcp://host-engine:1234")
	o := fixture(t)
	files := configFiles(t, o)
	top := files.topText + engineConfig("managed", "") + "\n[roles.mason]\nsandbox = 'sbx'\nimage = 'fixture-image'\n"
	files.write(t, top)
	intakefixture.Must(t, os.Remove(fake.Version))
	s, c := start(t, o)
	return &engineReload{fake: fake, files: files, top: top, s: s, c: c}
}

// reloadedWhileBusy holds a unit check and an engine turn, then reloads a
// configuration that expects v0.21.0.
func (e *engineReload) reloadedWhileBusy(t *testing.T) {
	t.Helper()
	intakefixture.Must(t, os.WriteFile(e.fake.DockerCommands, nil, 0600))
	held, err := e.s.checkSlots.acquire(t.Context(), CheckSlotRun{Project: intakefixture.Project, Workstream: intakefixture.Stream, Unit: "checking-unit"})
	intakefixture.Must(t, err)
	turn, err := e.s.checkSlots.startEngineTurn(t.Context(), EngineTurn{Project: intakefixture.Project, Workstream: intakefixture.Stream, Unit: "working-unit", Agent: "mason-working", Turn: "turn-1"})
	intakefixture.Must(t, err)
	e.releaseWork = func() { held(); turn() }
	t.Cleanup(e.releaseWork)
	e.next = strings.Replace(e.top, "version = 'v0.20.5'", "version = 'v0.21.0'", 1)
	e.files.write(t, e.next)
	_, err = e.c.Reload(t.Context())
	intakefixture.Must(t, err)
}

// Serial: startup, reload and commands share process Docker selectors.
func TestServiceStartCreatesTheManagedEngineAtTheConfiguredVersion(t *testing.T) {
	e := startManagedEngine(t)
	if !strings.Contains(e.fake.Commands(t), "registry.dagger.io/engine:v0.20.5") {
		t.Fatalf("startup did not create managed engine: %s", e.fake.Commands(t))
	}
}

// Serial: startup, reload and commands share process Docker selectors.
func TestMasonsRunOnTheManagedEngineWithoutCredentials(t *testing.T) {
	e := startManagedEngine(t)
	settings := e.s.current().RoleExecution(masonRole)
	if settings.Dagger == nil || settings.Dagger.Engine != "docker-container://osmia-dagger-engine" {
		t.Fatalf("mason default: %+v", settings)
	}
	data, _ := json.Marshal(settings)
	if strings.Contains(string(data), engineTestToken) || strings.Contains(string(data), "cloud") {
		t.Fatalf("mason credential settings: %s", data)
	}
}

// Serial: startup, reload and commands share process Docker selectors.
func TestEngineRestartAndPruneAreRefusedWhileEngineWorkRuns(t *testing.T) {
	e := startManagedEngine(t)
	e.reloadedWhileBusy(t)
	for _, action := range []string{"restart", "prune"} {
		_, err := e.c.EngineAction(t.Context(), action)
		var api *APIError
		if !errors.As(err, &api) || api.Code != Unavailable || !strings.Contains(api.Message, "checking-unit") || !strings.Contains(api.Message, "mason-working") {
			t.Fatalf("busy %s: %v", action, err)
		}
	}
}

// Serial: startup, reload and commands share process Docker selectors.
func TestReloadLeavesARunningEngineAloneAndReportsTheVersionMismatch(t *testing.T) {
	e := startManagedEngine(t)
	e.reloadedWhileBusy(t)
	commands := e.fake.Commands(t)
	for _, line := range strings.Split(commands, "\n") {
		if !strings.HasPrefix(line, "container inspect ") {
			t.Fatalf("reload touched running engine: %s", commands)
		}
	}
	status, err := e.c.EngineStatus(t.Context())
	intakefixture.Must(t, err)
	if status.Mode != config.DaggerEngineManaged || !status.Reachable || status.Version != "v0.20.5" || status.ExpectedVersion != "v0.21.0" || !status.ConfiguredVersionMismatch || status.CacheSizeBytes == nil || *status.CacheSizeBytes != 4096 {
		t.Fatalf("reloaded status: %+v", status)
	}
}

// Serial: startup, reload and commands share process Docker selectors.
func TestRejectedReloadLeavesChecksOnTheCurrentEngine(t *testing.T) {
	e := startManagedEngine(t)
	e.reloadedWhileBusy(t)
	e.files.write(t, strings.Replace(e.next, "engine = \"managed\"", "engine = 'bad-mode'", 1))
	if _, err := e.c.Reload(t.Context()); err == nil {
		t.Fatal("invalid reload accepted")
	}
	result, err := e.s.configuredChecks(e.s.current()).Check(t.Context(), t.TempDir(), checks.Identity{}, nil)
	intakefixture.Must(t, err)
	assertEngineCheckOutput(t, result.Output, "managed", "", "docker-container://osmia-dagger-engine", "")
}

// Serial: startup, reload and commands share process Docker selectors.
func TestEngineRestartOnceWorkEndsRecreatesTheEngineAtTheReloadedVersion(t *testing.T) {
	e := startManagedEngine(t)
	e.reloadedWhileBusy(t)
	e.releaseWork()
	_, err := e.c.EngineAction(t.Context(), "restart")
	intakefixture.Must(t, err)
	status, err := e.c.EngineStatus(t.Context())
	intakefixture.Must(t, err)
	if status.Version != "v0.21.0" || status.ConfiguredVersionMismatch || !status.VersionMismatch {
		t.Fatalf("restarted status: %+v", status)
	}
	commands := e.fake.Commands(t)
	for _, want := range []string{"stop osmia-dagger-engine", "rm osmia-dagger-engine", "osmia-dagger-engine-cache:/var/lib/dagger registry.dagger.io/engine:v0.21.0"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("restart missing %q: %s", want, commands)
		}
	}
}

// Serial: startup, reload and commands share process Docker selectors.
func TestEnginePruneSucceedsOnceWorkEnds(t *testing.T) {
	e := startManagedEngine(t)
	e.reloadedWhileBusy(t)
	e.releaseWork()
	_, err := e.c.EngineAction(t.Context(), "prune")
	intakefixture.Must(t, err)
}

// Serial: startup, reload and commands share process Docker selectors.
func TestEngineRestartAndPruneDoNotApplyToAnExternalEngine(t *testing.T) {
	for _, mode := range []string{"auto", "tcp://external:1234", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			e := startManagedEngine(t)
			e.files.write(t, e.files.topText+engineConfig(mode, "OSMIA_ENGINE_TEST_TOKEN"))
			_, err := e.c.Reload(t.Context())
			intakefixture.Must(t, err)
			before := e.fake.Commands(t)
			for _, action := range []string{"restart", "prune"} {
				_, err = e.c.EngineAction(t.Context(), action)
				var api *APIError
				if !errors.As(err, &api) || api.Code != Conflict || !strings.Contains(api.Message, "not applicable") {
					t.Fatalf("mode %s %s: %v", mode, action, err)
				}
			}
			status, err := e.c.EngineStatus(t.Context())
			intakefixture.Must(t, err)
			if !status.Reachable || status.Version == "" || status.CacheSizeBytes == nil {
				t.Fatalf("%s status: %+v", mode, status)
			}
			if e.fake.Commands(t) != before {
				t.Fatalf("%s touched docker", mode)
			}
		})
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
			files.write(t, files.topText+engineConfig(mode, ""))
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
			files.write(t, files.topText)
			s, c := start(t, o)
			before := s.current()
			files.write(t, files.topText+engineConfig(mode, ""))
			_, err = c.Reload(t.Context())
			assertConflict(err)
			if s.current() != before {
				t.Fatal("failed reload replaced loaded configuration")
			}
			t.Setenv("DOCKER_CONTEXT", "default")
			_, err = c.Reload(t.Context())
			intakefixture.Must(t, err)
			t.Setenv("DOCKER_CONTEXT", "named")
			intakefixture.Must(t, os.WriteFile(fake.Environments, nil, 0600))
			status, err := c.EngineStatus(t.Context())
			intakefixture.Must(t, err)
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
			intakefixture.Must(t, err)
			if len(log) != 0 {
				t.Fatalf("conflict reached engine: %s", log)
			}
		})
	}
}

type armedDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func (c *armedDeadline) Done() <-chan struct{} { return c.done }

func (c *armedDeadline) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *armedDeadline) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

func deadlineAfter(parent context.Context, armed <-chan struct{}, delay time.Duration) (context.Context, context.CancelFunc) {
	c := &armedDeadline{Context: parent, done: make(chan struct{})}
	stop := make(chan struct{})
	go func() {
		var expired <-chan time.Time
		for {
			select {
			case <-armed:
				timer := time.NewTimer(delay)
				defer timer.Stop()
				expired, armed = timer.C, nil
			case <-expired:
				c.finish(context.DeadlineExceeded)
				return
			case <-parent.Done():
				c.finish(parent.Err())
				return
			case <-stop:
				c.finish(context.Canceled)
				return
			}
		}
	}()
	var once sync.Once
	return c, func() { once.Do(func() { close(stop) }) }
}

// Serial: the API invokes fake Docker through the process PATH.
func TestCreatingEngineAPIRespondsAtDeadline(t *testing.T) {
	fake := fixturekit.InstallEngineCLIs(t)
	t.Setenv("DOCKER_HOST", "tcp://engine.example.ts.net:2375")
	t.Setenv("DOCKER_CONTEXT", "default")
	o := fixture(t)
	files := configFiles(t, o)
	files.write(t, files.topText+engineConfig("managed", ""))
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
	intakefixture.Must(t, os.WriteFile(filepath.Join(fake.Directory, "docker"), []byte(script), 0700))
	creating := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := deadlineAfter(r.Context(), creating, 500*time.Millisecond)
		defer cancel()
		s.engineHTTP(w, r.WithContext(ctx))
	}))
	defer server.Close()
	completed := make(chan error, 1)
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
	started := time.Now()
	close(creating)
	next := queuedSlot(t.Context(), s.checkSlots, CheckSlotRun{Unit: "waiting-for-creation"})
	intakefixture.Must(t, awaitCheck(t, completed))
	if time.Since(started) > 2*time.Second {
		t.Fatal("API did not deliver response at the action bound")
	}
	release := awaitCheck(t, next)
	if release == nil {
		t.Fatal("API action retained maintenance gate")
	}
	release()
}
