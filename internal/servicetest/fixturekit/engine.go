package fixturekit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EngineCLIs installs local fake engine commands. The version file models the
// managed container's image; DockerCommands records every lifecycle invocation.
type EngineCLIs struct{ Directory, Version, DockerCommands string }

func InstallEngineCLIs(t *testing.T) EngineCLIs {
	t.Helper()
	dir := t.TempDir()
	f := EngineCLIs{Directory: dir, Version: filepath.Join(dir, "version"), DockerCommands: filepath.Join(dir, "docker-commands")}
	Must(t, os.WriteFile(f.Version, []byte("v0.20.5"), 0600))
	docker := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> %q
case "$1" in
 container)
  if [ ! -f %q ]; then echo 'No such container: osmia-dagger-engine' >&2; exit 1; fi
  v=$(cat %q)
  printf '[{"Config":{"Image":"registry.dagger.io/engine:%%s"},"State":{"Running":true},"Mounts":[{"Type":"volume","Name":"osmia-dagger-engine-cache","Destination":"/var/lib/dagger"}]}]\n' "$v"
  ;;
 run)
  for arg do image=$arg; done
  printf '%%s' "${image##*:}" > %q
  ;;
 stop|rm|start) ;;
 *) exit 92 ;;
esac
`, f.DockerCommands, f.Version, f.Version, f.Version)
	dagger := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
 version) echo 'dagger v0.20.5'; exit 0 ;;
 core)
  case "$2" in
   version) if [ -f %q ]; then cat %q; else echo v0.20.5; fi ;;
   engine) if [ "${4-}" != prune ]; then echo 4096; fi ;;
   *) exit 93 ;;
  esac
  exit 0 ;;
 list) printf 'dag://go/test\n'; exit 0 ;;
 check) ;;
 *) exit 94 ;;
esac
printf 'ENGINE=%%s\nRUNNER=%%s\nDOCKER_HOST=%%s\nCI=%%s\nTOKEN_PRESENT=%%s\n' "${DAGGER_ENGINE-}" "${_EXPERIMENTAL_DAGGER_RUNNER_HOST-}" "${DOCKER_HOST-}" "${CI-}" "${DAGGER_CLOUD_TOKEN:+true}"
[ -z "${GITHUB_TOKEN-}${OSMIA_ENGINE_TEST_TOKEN-}" ] || exit 95
if git rev-parse --verify HEAD >/dev/null 2>&1; then
 printf 'HEAD=%%s\nBRANCH=%%s\nORIGIN=%%s\n' "$(git rev-parse HEAD)" "$(git symbolic-ref --short HEAD)" "$(git remote get-url origin)"
else
 echo ANONYMOUS_EXPORT
fi
if [ -n "${DAGGER_CLOUD_TOKEN-}" ]; then printf '%%s\n' "$DAGGER_CLOUD_TOKEN" >&2; fi
echo 'View trace at https://dagger.cloud/example/traces/service123' >&2
printf '\n== CHECKS ==  ✔ 1 passed\n✔ dag://go/test 1s OK\n'
`, f.Version, f.Version)
	Must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0700))
	Must(t, os.WriteFile(filepath.Join(dir, "dagger"), []byte(dagger), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f EngineCLIs) Commands(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.DockerCommands)
	if os.IsNotExist(err) {
		return ""
	}
	Must(t, err)
	return strings.TrimSpace(string(b))
}
