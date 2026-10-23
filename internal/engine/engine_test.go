package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// The executables maintain local container/cache state and reject every command
// outside the engine-control protocol. They have no access to a real engine.
func fakeHost(t *testing.T) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("docker", `#!/bin/sh
set -eu
cd "$(dirname "$0")"
printf 'docker %s\n' "$*" >> commands
case "$*" in
 'container inspect osmia-dagger-engine')
  if [ -f docker-error ]; then echo 'daemon unavailable' >&2; exit 1; fi
  if [ ! -f state ]; then echo 'Error: No such container: osmia-dagger-engine' >&2; exit 1; fi
  running=$(cat state)
  image=$(cat image)
  volume=$(cat volume)
  printf '[{"State":{"Running":%s},"Config":{"Image":"%s"},"Mounts":[{"Type":"volume","Name":"%s","Destination":"/var/lib/dagger"}]}]\n' "$running" "$image" "$volume"
  ;;
 'start osmia-dagger-engine') echo true > state ;;
 'stop osmia-dagger-engine') echo false > state ;;
 'rm osmia-dagger-engine') rm state image ;;
 run\ *)
  test ! -f state
  printf '%s\n' "$*" > run-args
  for arg do
   case "$arg" in
    registry.dagger.io/engine:*) echo "$arg" > image ;;
    *:/var/lib/dagger) echo "${arg%:/var/lib/dagger}" > volume ;;
   esac
  done
  echo true > state
  ;;
 *) echo "unexpected docker command: $*" >&2; exit 2 ;;
esac
`)
	write("dagger", `#!/bin/sh
set -eu
cd "$(dirname "$0")"
printf 'dagger %s\n' "$*" >> commands
case "$*" in
 version) echo "version: ${DAGGER_X_RELEASE:-v1.0.0-beta.17}"; exit 0 ;;
esac
printf '%s\n' "DAGGER_ENGINE=${DAGGER_ENGINE-}" "DAGGER_CLOUD_ENGINE=${DAGGER_CLOUD_ENGINE-}" "_EXPERIMENTAL_DAGGER_RUNNER_HOST=${_EXPERIMENTAL_DAGGER_RUNNER_HOST-}" "DAGGER_CLOUD_TOKEN=${DAGGER_CLOUD_TOKEN-}" "DAGGER_X_RELEASE=${DAGGER_X_RELEASE-}" "CI=${CI-}" "GITHUB_TOKEN=${GITHUB_TOKEN-}" > probe-env
if [ -f unreachable ]; then echo "connection refused ${DAGGER_CLOUD_TOKEN-}" >&2; exit 1; fi
case "$*" in
 'core version')
  if [ -f image ]; then cat image | sed 's/.*engine://' ; else echo v1.0.0-beta.17; fi ;;
 'core engine local-cache entry-set disk-space-bytes')
  if [ -f cache-unknown ]; then exit 1; fi
  if [ -f cache ]; then cat cache; else echo 4096; fi ;;
 'core engine local-cache prune') echo 0 > cache ;;
 *) echo "unexpected dagger command: $*" >&2; exit 2 ;;
esac
`)
	o := Options{LookPath: func(name string) (string, error) {
		if name != "docker" && name != "dagger" {
			return "", exec.ErrNotFound
		}
		return filepath.Join(dir, name), nil
	},
		Getenv: func(name string) string {
			if name == "PATH" {
				return os.Getenv("PATH")
			}
			return ""
		}}
	return o, dir
}

func put(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}
func managed(version string) config.ResolvedDagger {
	return config.DaggerConfig{Engine: "managed", Version: version}.Resolve()
}
func running(t *testing.T, dir, version string, active bool) {
	t.Helper()
	state := "false"
	if active {
		state = "true"
	}
	put(t, dir, "state", state)
	put(t, dir, "image", "registry.dagger.io/engine:"+version)
	put(t, dir, "volume", CacheVolumeName)
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestEnsureManaged(t *testing.T) {
	for _, state := range []string{"absent", "stopped", "running", "different version"} {
		t.Run(state, func(t *testing.T) {
			o, dir := fakeHost(t)
			version := "v1.0.0-beta.17"
			if state != "absent" {
				current := version
				if state == "different version" {
					current = "v0.20.5"
				}
				running(t, dir, current, state != "stopped")
			}
			c := New(managed(version), o)
			must(t, c.Ensure(context.Background()))
			commands := read(t, dir, "commands")
			switch state {
			case "absent":
				want := "docker container inspect osmia-dagger-engine\ndocker run --detach --name osmia-dagger-engine --privileged --restart always --volume " + CacheVolumeName + ":/var/lib/dagger registry.dagger.io/engine:" + version
				if commands != want {
					t.Fatalf("commands: %s", commands)
				}
			case "stopped":
				if commands != "docker container inspect osmia-dagger-engine\ndocker start osmia-dagger-engine" {
					t.Fatal(commands)
				}
			default:
				if commands != "docker container inspect osmia-dagger-engine" {
					t.Fatal(commands)
				}
			}
			s, err := c.Status(context.Background())
			must(t, err)
			wantVersion := version
			if state == "different version" {
				wantVersion = "v0.20.5"
			}
			if !s.Reachable || s.Version != wantVersion || s.ExpectedVersion != version || s.ConfiguredVersionMismatch != (state == "different version") {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestExpectedVersionDefaultsToCLI(t *testing.T) {
	o, dir := fakeHost(t)
	c := New(managed(""), o)
	version, err := c.ExpectedVersion(context.Background())
	must(t, err)
	if version != "v1.0.0-beta.17" {
		t.Fatal(version)
	}
	must(t, c.Ensure(context.Background()))
	if !strings.HasSuffix(read(t, dir, "run-args"), "registry.dagger.io/engine:"+version) {
		t.Fatal(read(t, dir, "run-args"))
	}
	s, err := c.Status(context.Background())
	must(t, err)
	if s.ExpectedVersion != version {
		t.Fatalf("%+v", s)
	}
}

func TestHostReleaseSelectionInEveryMode(t *testing.T) {
	for _, mode := range []string{"managed", "auto", "tcp://engine:1234", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			o, dir := fakeHost(t)
			running(t, dir, "v0.22.0-beta.3", true)
			baseGetenv := o.Getenv
			o.Getenv = func(name string) string {
				switch name {
				case "DAGGER_X_RELEASE":
					return "v0.22.0-beta.3"
				case "MY_CLOUD_TOKEN":
					return "private-probe-token"
				}
				return baseGetenv(name)
			}
			cfg := config.DaggerConfig{Engine: mode, CloudTokenEnv: "MY_CLOUD_TOKEN"}.Resolve()
			s, err := New(cfg, o).Status(context.Background())
			must(t, err)
			if s.CLIVersion != "v0.22.0-beta.3" || s.ExpectedVersion != "v0.22.0-beta.3" || s.Version != "v0.22.0-beta.3" || s.VersionMismatch || s.ConfiguredVersionMismatch {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestEnsureInspectionFailureLeavesContainerUntouched(t *testing.T) {
	o, dir := fakeHost(t)
	put(t, dir, "docker-error", "yes")
	if err := New(managed("v0.20.5"), o).Ensure(context.Background()); err == nil {
		t.Fatal("daemon failure treated as a missing container")
	}
	if read(t, dir, "commands") != "docker container inspect osmia-dagger-engine" {
		t.Fatal(read(t, dir, "commands"))
	}
}

func TestStatusEveryMode(t *testing.T) {
	for _, mode := range []string{"managed", "auto", "tcp://engine:1234", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			o, dir := fakeHost(t)
			running(t, dir, "v0.20.5", true)
			env := map[string]string{"DAGGER_ENGINE": "tcp://wrong:1000", "DAGGER_CLOUD_ENGINE": "wrong-cloud", "_EXPERIMENTAL_DAGGER_RUNNER_HOST": "tcp://wrong:2000", "DAGGER_X_RELEASE": "v0.1.0", "MY_CLOUD_TOKEN": "private-probe-token", "GITHUB_TOKEN": "unrelated-secret", "CI": "true"}
			baseGetenv := o.Getenv
			o.Getenv = func(name string) string {
				if v, ok := env[name]; ok {
					return v
				}
				return baseGetenv(name)
			}
			cfg := config.DaggerConfig{Engine: mode, Version: "v1.0.0-beta.17", CloudTokenEnv: "MY_CLOUD_TOKEN"}.Resolve()
			s, err := New(cfg, o).Status(context.Background())
			must(t, err)
			if s.Mode != cfg.Mode || s.Address != cfg.Address || !s.Reachable || s.Version != "v0.20.5" || s.CLIVersion != "v0.1.0" || !s.VersionMismatch || !s.ConfiguredVersionMismatch || s.CacheSizeBytes == nil || *s.CacheSizeBytes != 4096 {
				t.Fatalf("%+v", s)
			}
			commands := read(t, dir, "commands")
			if mode != "managed" && strings.Contains(commands, "docker") {
				t.Fatal(commands)
			}
			gotEnv := read(t, dir, "probe-env")
			selection := "DAGGER_ENGINE=" + cfg.Address
			legacy := "_EXPERIMENTAL_DAGGER_RUNNER_HOST=" + cfg.Address
			cloud, token := "DAGGER_CLOUD_ENGINE=", "DAGGER_CLOUD_TOKEN="
			switch mode {
			case "auto":
				selection = "DAGGER_ENGINE=tcp://wrong:1000"
				legacy = "_EXPERIMENTAL_DAGGER_RUNNER_HOST=tcp://wrong:2000"
				cloud = "DAGGER_CLOUD_ENGINE=wrong-cloud"
			case "cloud":
				selection = "DAGGER_ENGINE=cloud"
				legacy = "_EXPERIMENTAL_DAGGER_RUNNER_HOST="
				cloud = "DAGGER_CLOUD_ENGINE=1"
				token = "DAGGER_CLOUD_TOKEN=private-probe-token"
			}
			wantEnv := strings.Join([]string{selection, cloud, legacy, token, "DAGGER_X_RELEASE=v0.1.0", "CI=", "GITHUB_TOKEN="}, "\n")
			if gotEnv != wantEnv {
				t.Fatalf("probe env %q, want %q", gotEnv, wantEnv)
			}
		})
	}
}

func TestRestartPreservesCacheAndAppliesVersion(t *testing.T) {
	o, dir := fakeHost(t)
	running(t, dir, "v0.20.5", true)
	put(t, dir, "volume", "existing-engine-cache")
	put(t, dir, "cache", "8192")
	c := New(managed("v1.0.0-beta.17"), o)
	must(t, c.Restart(context.Background()))
	commands := strings.Split(read(t, dir, "commands"), "\n")
	want := []string{"docker container inspect osmia-dagger-engine", "docker stop osmia-dagger-engine", "docker rm osmia-dagger-engine", "docker run --detach --name osmia-dagger-engine --privileged --restart always --volume existing-engine-cache:/var/lib/dagger registry.dagger.io/engine:v1.0.0-beta.17"}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("%v", commands)
	}
	if read(t, dir, "cache") != "8192" || read(t, dir, "volume") != "existing-engine-cache" {
		t.Fatal("lost cache on restart")
	}
	must(t, c.Prune(context.Background()))
	s, err := c.Status(context.Background())
	must(t, err)
	if s.Version != "v1.0.0-beta.17" || s.VersionMismatch || s.ConfiguredVersionMismatch || s.CacheSizeBytes == nil || *s.CacheSizeBytes != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestNonManagedActionsAreNotApplicable(t *testing.T) {
	for _, mode := range []string{"auto", "cloud", "tcp://engine:1234"} {
		t.Run(mode, func(t *testing.T) {
			c := New(config.DaggerConfig{Engine: mode}.Resolve(), Options{LookPath: func(string) (string, error) { t.Fatal("non-managed action looked for an executable"); return "", nil }})
			must(t, c.Ensure(context.Background()))
			for _, action := range []func(context.Context) error{c.Restart, c.Prune} {
				var refusal *NotApplicableError
				err := action(context.Background())
				if !errors.As(err, &refusal) || refusal.Mode != c.config.Mode || !strings.Contains(err.Error(), "not applicable to this mode") {
					t.Fatalf("%v", err)
				}
			}
		})
	}
}

func TestStatusUnreachableAndUnknownCache(t *testing.T) {
	for _, state := range []string{"absent", "stopped", "unreachable", "unknown cache"} {
		t.Run(state, func(t *testing.T) {
			o, dir := fakeHost(t)
			if state != "absent" {
				running(t, dir, "v0.20.5", state != "stopped")
			}
			if state == "unreachable" {
				put(t, dir, "unreachable", "yes")
			}
			if state == "unknown cache" {
				put(t, dir, "cache-unknown", "yes")
			}
			c := New(managed("v0.20.5"), o)
			s, err := c.Status(context.Background())
			must(t, err)
			if s.Reachable != (state == "unknown cache") || s.CacheSizeBytes != nil {
				t.Fatalf("%+v", s)
			}
			if !s.Reachable && s.ReachabilityError == "" {
				t.Fatalf("%+v", s)
			}
			commands := read(t, dir, "commands")
			for _, mutation := range []string{"docker start", "docker run", "docker stop", "docker rm", "local-cache prune"} {
				if strings.Contains(commands, mutation) {
					t.Fatal(commands)
				}
			}
			if state == "stopped" || state == "absent" {
				if strings.Contains(commands, "dagger core") {
					t.Fatal(commands)
				}
			}
		})
	}
}

func TestCloudStatusMissingTokenAndRedaction(t *testing.T) {
	o, dir := fakeHost(t)
	cfg := config.DaggerConfig{Engine: "cloud", CloudTokenEnv: "MY_TOKEN"}.Resolve()
	s, err := New(cfg, o).Status(context.Background())
	must(t, err)
	if s.Reachable || !strings.Contains(s.ReachabilityError, "token is not set") {
		t.Fatalf("%+v", s)
	}
	if strings.Contains(read(t, dir, "commands"), "dagger core") {
		t.Fatal("missing token connected to engine")
	}
	put(t, dir, "unreachable", "yes")
	get := o.Getenv
	o.Getenv = func(name string) string {
		if name == "MY_TOKEN" {
			return "cloud-secret-test"
		}
		return get(name)
	}
	s, err = New(cfg, o).Status(context.Background())
	must(t, err)
	if strings.Contains(s.ReachabilityError, "cloud-secret-test") || !strings.Contains(s.ReachabilityError, "[redacted]") {
		t.Fatalf("%+v", s)
	}
}

func TestProbesHaveDeadline(t *testing.T) {
	calls := 0
	o := Options{LookPath: func(name string) (string, error) { return name, nil }, Command: func(ctx context.Context, _ string, _ []string, args ...string) (string, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatal("unbounded probe")
		}
		return "v0.20.5", nil
	}, Timeout: time.Second}
	_, err := New(config.DaggerConfig{}.Resolve(), o).Status(context.Background())
	must(t, err)
	if calls != 3 {
		t.Fatalf("%d calls", calls)
	}
}

func TestReleaseFormats(t *testing.T) {
	for _, text := range []string{"dagger v0.20.5 (linux/arm64)", "version: v0.20.5\ncommit: abcd", "v0.20.5", "0.20.5"} {
		if got := release(text); got != "v0.20.5" {
			t.Fatalf("%q: %q", text, got)
		}
	}
	if imageRelease("registry.dagger.io/engine@sha256:123") != "" {
		t.Fatal("digest treated as release")
	}
}

func TestRestartAbsentAndStopped(t *testing.T) {
	for _, state := range []string{"absent", "stopped"} {
		t.Run(state, func(t *testing.T) {
			o, dir := fakeHost(t)
			if state == "stopped" {
				running(t, dir, "v0.20.4", false)
			}
			must(t, New(managed("v0.20.5"), o).Restart(context.Background()))
			commands := read(t, dir, "commands")
			if strings.Contains(commands, "docker stop") || !strings.Contains(commands, "docker run") {
				t.Fatal(commands)
			}
			if read(t, dir, "image") != "registry.dagger.io/engine:v0.20.5" {
				t.Fatal("restart did not apply configured release")
			}
		})
	}
}

func TestPruneDoesNotStartEngine(t *testing.T) {
	for _, state := range []string{"absent", "stopped"} {
		t.Run(state, func(t *testing.T) {
			o, dir := fakeHost(t)
			if state == "stopped" {
				running(t, dir, "v0.20.4", false)
			}
			if err := New(managed("v0.20.5"), o).Prune(context.Background()); err == nil {
				t.Fatal("pruned a stopped engine")
			}
			if read(t, dir, "commands") != "docker container inspect osmia-dagger-engine" {
				t.Fatal(read(t, dir, "commands"))
			}
		})
	}
}

func TestProbeTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dagger")
	must(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 10\n"), 0700))
	c := New(config.DaggerConfig{}.Resolve(), Options{LookPath: func(string) (string, error) { return path, nil }, Timeout: 10 * time.Millisecond})
	started := time.Now()
	_, err := c.ExpectedVersion(context.Background())
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("%v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("probe did not respect timeout")
	}
}
