package checks

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// serial: installs a fake CLI and plants host credentials and engine selectors.
func TestDaggerClientEnvironment(t *testing.T) {
	bin := t.TempDir()
	// Remove shell-added variables before dumping the client environment.
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\nunset PWD SHLVL _\nexec env -0\n"), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, value := range map[string]string{
		"GITHUB_TOKEN": "host-github-secret", "GITHUB_ACTIONS": "true", "GITHUB_RUN_ID": "123", "CI": "host-ci",
		"ANTHROPIC_API_KEY": "host-provider-secret", "SSH_AUTH_SOCK": "/host/agent", "DAGGER_CLOUD_TOKEN": "host-cloud-secret",
		"DAGGER_ENGINE": "cloud", "DAGGER_CLOUD_ENGINE": "1", "DOCKER_HOST": "tcp://host-docker:1234",
		"_EXPERIMENTAL_DAGGER_RUNNER_HOST": "tcp://host-runner:1234", "DAGGER_X_RELEASE": "test-release",
	} {
		t.Setenv(name, value)
	}
	clone := t.TempDir()
	git := func(args ...string) string {
		return strings.TrimSpace(fixturekit.DemoGit(t, bin, append([]string{"-C", clone}, args...)...))
	}
	git("init", "--quiet")
	git("-c", "user.name=Candidate", "-c", "user.email=candidate@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "candidate")
	identity := Identity{Candidate: git("rev-parse", "HEAD"), Branch: "osmia-unit/example/unit", Remote: "https://github.com/example/project", Repository: clone}
	for _, engine := range []string{"", "auto", "tcp://configured:1234", "docker-container://osmia-dagger-engine", "cloud"} {
		for _, token := range []string{"", "client-environment-secret"} {
			for _, fallback := range []bool{false, true} {
				label := engine
				if token != "" {
					label += "/authenticated"
				} else {
					label += "/anonymous"
				}
				if fallback {
					label += "/fallback"
				}
				t.Run(label, func(t *testing.T) {
					d := DaggerChecks{Engine: engine, CloudToken: token}
					id := identity
					if fallback {
						id.Candidate = "unresolved"
					}
					var logs bytes.Buffer
					ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
					for _, operation := range []string{"check", "list", "call", "core"} {
						var output bytes.Buffer
						exit, prep, err := d.dagger(ctx, t.TempDir(), id, operation == "check" && token != "", &output, operation)
						if engine == "cloud" && token == "" {
							if err == nil || exit != -1 || output.Len() != 0 {
								t.Fatal("Cloud ran without a configured token")
							}
							continue
						}
						must(t, err)
						if exit != 0 {
							t.Fatalf("fake exited %d: %s", exit, output.String())
						}
						got := map[string]string{}
						for _, entry := range strings.Split(strings.TrimSuffix(output.String(), "\x00"), "\x00") {
							name, value, ok := strings.Cut(entry, "=")
							if !ok {
								t.Fatalf("invalid environment entry %q", entry)
							}
							if _, exists := got[name]; exists {
								t.Fatalf("duplicate %s", name)
							}
							got[name] = value
						}
						home := got["HOME"]
						if home == "" || home == os.Getenv("HOME") {
							t.Fatal("no isolated home")
						}
						want := map[string]string{"PATH": os.Getenv("PATH"), "HOME": home, "TMPDIR": home, "NO_COLOR": "1", "DAGGER_X_RELEASE": "test-release"}
						if engine == "" || engine == "auto" {
							want["DOCKER_HOST"] = "tcp://host-docker:1234"
							want["_EXPERIMENTAL_DAGGER_RUNNER_HOST"] = "tcp://host-runner:1234"
						} else {
							want["DAGGER_ENGINE"] = engine
							if engine != "cloud" {
								want["_EXPERIMENTAL_DAGGER_RUNNER_HOST"] = engine
							}
						}
						if token != "" && (operation == "check" || operation == "call" || operation == "core" || engine == "cloud") {
							want["DAGGER_CLOUD_TOKEN"] = token
						}
						if token != "" && operation == "check" && !fallback {
							want["CI"] = "true"
						}
						if !reflect.DeepEqual(got, want) {
							// Names and the redacted dump diagnose failures without logging a token.
							t.Fatalf("environment mismatch: %s", redact.Secrets(output.String(), redact.KnownSecrets()...))
						}
						if operation == "check" && token != "" && (prep.CIIdentity == fallback || (prep.FallbackReason != "") != fallback) {
							t.Fatalf("preparation %+v", prep)
						}
					}
					if token != "" && !slices.Contains(redact.KnownSecrets(), token) {
						t.Fatal("token was not registered")
					}
					if token != "" && strings.Contains(logs.String(), token) {
						t.Fatal("token leaked into logs")
					}
				})
			}
		}
	}
}

// serial: installs a fake CLI that prints the token and a trace URL.
func TestDaggerClientRedactsOutputAndCapturesTrace(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const token = "client-output-secret"
	script := `#!/bin/sh
printf '%s\n' "$DAGGER_CLOUD_TOKEN" >&2
printf 'View trace at https://dagger.cloud/example/traces/abc123\n' >&2
printf 'CHECKS  ✔ 1 passed\n✔ dag://go/test 1s OK\n'
`
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
	var logs bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
	result, err := (DaggerChecks{Engine: "cloud", CloudToken: token}).Check(ctx, t.TempDir(), Identity{Candidate: token}, nil)
	must(t, err)
	if result.ExitCode != 0 || result.TraceURL != "https://dagger.cloud/example/traces/abc123" || !strings.Contains(result.Output, "[redacted]") {
		t.Fatalf("result %+v", result)
	}
	if strings.Contains(result.Output+result.CIIdentityFallback+logs.String(), token) {
		t.Fatal("token leaked")
	}
	// Listings have a separate error path and output budget.
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte("#!/bin/sh\nprintf '%s' \"$DAGGER_CLOUD_TOKEN\" >&2\nexit 1\n"), 0700))
	_, err = (DaggerChecks{Engine: "cloud", CloudToken: token}).List(ctx, t.TempDir(), Identity{})
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("listing error %v", err)
	}
}

func TestCloudTraceURL(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"", "https://dagger.cloud/docs", "http://dagger.cloud/org/traces/abc", "https://dagger.cloud.evil/org/traces/abc", "https://secret@dagger.cloud/org/traces/abc", "https://dagger.cloud/org/traces/abc?token=secret", "https://dagger.cloud/org/traces/abc#secret", "https://dagger.cloud:443/org/traces/abc"} {
		if got := cloudTraceURL(output); got != "" {
			t.Fatalf("accepted %q", output)
		}
	}
	for _, link := range []string{"https://dagger.cloud/org/traces/abc-123", "https://dagger.cloud/traces/abc123"} {
		if got := cloudTraceURL("View trace at (" + link + ").\n"); got != link {
			t.Fatalf("got %q", got)
		}
	}
}
