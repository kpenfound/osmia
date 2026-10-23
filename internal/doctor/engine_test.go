package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestDoctorManagedEngine(t *testing.T) {
	for _, state := range []string{"reachable", "unreachable", "mismatch"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			docker := filepath.Join(dir, "docker")
			dagger := filepath.Join(dir, "dagger")
			must(t, os.WriteFile(docker, []byte(`#!/bin/sh
if [ "$*" != 'container inspect osmia-dagger-engine' ]; then echo 'doctor attempted a mutation' >&2; exit 1; fi
printf '[{"State":{"Running":true},"Config":{"Image":"registry.dagger.io/engine:v0.20.5"}}]\n'
`), 0700))
			version := "v0.20.5"
			if state == "mismatch" {
				version = "v0.20.4"
			}
			probe := `#!/bin/sh
case "$*" in
 version) echo 'dagger v0.20.5'; exit 0 ;;
esac
if [ "${_EXPERIMENTAL_DAGGER_RUNNER_HOST-}" != docker-container://osmia-dagger-engine ]; then echo 'incorrect engine' >&2; exit 1; fi
case "$*" in
 'core version') echo '` + version + `' ;;
 'core engine local-cache entry-set disk-space-bytes') echo 8192 ;;
 *) exit 1 ;;
esac
`
			if state == "unreachable" {
				probe = strings.Replace(probe, "case \"$*\" in\n 'core version')", "echo 'engine does not answer' >&2; exit 1\ncase \"$*\" in\n 'core version')", 1)
			}
			must(t, os.WriteFile(dagger, []byte(probe), 0700))
			// Run the public doctor flow against a root with no projects, fake tools,
			// and the actual engine subprocess boundary.
			root, err := config.ResolveRoot(filepath.Join(dir, "root"), "")
			must(t, err)
			must(t, os.MkdirAll(root.String(), 0700))
			must(t, os.WriteFile(filepath.Join(root.String(), "config.toml"), []byte(`version = 1
[dagger]
engine = "managed"
[profiles.default]
agent = "claude"
model = "opus"
`), 0600))
			h := host{}
			o := Options{Root: root, LookPath: func(name string) (string, error) {
				switch name {
				case "docker":
					return docker, nil
				case "dagger":
					return dagger, nil
				}
				return h.lookPath(name)
			}, Command: h.command, CheckJJ: func(context.Context) (string, error) { return "0.45.1", nil }, Getenv: os.Getenv}
			// Leave EngineCommand on the real subprocess implementation, rather than
			// the generic fake toolchain Command.
			o.EngineCommand = func(ctx context.Context, path string, env []string, args ...string) (string, error) {
				cmd := exec.CommandContext(ctx, path, args...)
				cmd.Env = env
				out, err := cmd.Output()
				return string(out), err
			}
			checks := Run(context.Background(), o)
			reach := find(t, checks, "dagger engine")
			versionCheck := find(t, checks, "dagger engine version")
			cache := find(t, checks, "dagger engine cache")
			if state == "unreachable" {
				expect(t, reach, Warn, "managed: unreachable")
				expect(t, cache, Pass, "unknown")
			} else {
				expect(t, reach, Pass, "managed: reachable")
				expect(t, cache, Pass, "8192 bytes")
			}
			if state == "mismatch" {
				expect(t, versionCheck, Warn, "engine v0.20.4; expected v0.20.5; host CLI v0.20.5")
				if !strings.Contains(versionCheck.Detail, "releases differ") {
					t.Fatal(versionCheck)
				}
			} else {
				expect(t, versionCheck, Pass, "engine v0.20.5; expected v0.20.5; host CLI v0.20.5")
			}
			if Failed(checks) {
				t.Fatalf("engine warnings caused failure: %+v", checks)
			}
		})
	}
}
