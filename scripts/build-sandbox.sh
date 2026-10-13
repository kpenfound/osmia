#!/usr/bin/env bash
set -euo pipefail

readonly TEMPLATE='osmia:latest'
readonly DAGGER_VERSION='1.0.0-beta.17'

if [[ $# -ne 0 ]]; then
    printf 'Usage: %s\n' "$0" >&2
    exit 2
fi
command -v sbx >/dev/null || { printf 'sbx is required on PATH.\n' >&2; exit 1; }

build_dir=$(mktemp -d "${TMPDIR:-/tmp}/osmia-build.XXXXXXXX")
readonly sandbox="$(basename "$build_dir")"
cleanup() {
    local status=$?
    trap - EXIT
    if ! sbx rm --force "$sandbox"; then
        printf 'Could not remove temporary sandbox %s; run: sbx rm --force %s\n' "$sandbox" "$sandbox" >&2
        status=1
    fi
    rmdir "$build_dir"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# No paths are supplied, so the build has no mounted workspace.
sbx create --name "$sandbox" --skills off --pull always codex
sbx exec "$sandbox" npm install -g @openai/codex@latest @anthropic-ai/claude-code@latest
sbx exec -i -u root "$sandbox" env DAGGER_VERSION="$DAGGER_VERSION" BIN_DIR=/usr/local/bin CI=1 sh -eu <<'INSTALL'
# The Claude kit writes settings as agent when creating a sandbox from this image.
install -d -o agent -g agent /home/agent/.claude
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
curl -fsSL https://dl.dagger.io/dagger/install.sh -o "$installer"
sh "$installer"
INSTALL

sbx exec "$sandbox" codex --version
sbx exec "$sandbox" claude --version
dagger_version=$(sbx exec "$sandbox" dagger version)
printf '%s\n' "$dagger_version"
if [[ "$(awk '$1 == "version:" {print $2}' <<<"$dagger_version")" != "v${DAGGER_VERSION}" ]]; then
    printf 'Expected Dagger v%s; refusing to save template.\n' "$DAGGER_VERSION" >&2
    exit 1
fi

sbx stop "$sandbox"
sbx template save "$sandbox" "$TEMPLATE"
printf 'Saved local template %s with Dagger v%s.\n' "$TEMPLATE" "$DAGGER_VERSION"
