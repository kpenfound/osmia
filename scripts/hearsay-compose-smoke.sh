#!/usr/bin/env bash
# Runs Osmia's publisher and read path against a real Hearsay under Compose.
#
#   scripts/hearsay-compose-smoke.sh [evidence-dir]
#
# Run it from the repository root with Docker and `docker compose`. It
#
#   1. builds Hearsay from the tree under test (HEARSAY_REF, or HEARSAY_SRC)
#      into an image shaped like the one Hearsay publishes,
#   2. writes the Hearsay configuration `osmia project memory` generates for the
#      run's project, with the stub model tiers and one outside principal,
#   3. validates it with `hearsay config validate`, and starts Hearsay in
#      Hearsay's own deploy/compose shape (Postgres, migrate, connectors,
#      distiller, assert-worker, api),
#   4. runs TestHearsaySmokeRun (internal/service/hearsay_smoke_test.go) in a
#      container on that network: the service's publisher pass posts a sealed
#      spec, an owner ruling and a chief assumption, and Osmia's own read tools
#      and bundle provider read them back as each class agent acting for the
#      owner, and as an outsider.
#
# Everything runs in containers, so the output is container evidence: it says
# what ran where. The evidence directory (default: a new temporary directory,
# printed at the end) holds the raw requests and answers.
#
# Environment:
#   HEARSAY_REF   Hearsay commit to build (default: the commit the work was built against)
#   HEARSAY_SRC   an existing Hearsay tree to build instead of downloading HEARSAY_REF
#   KEEP=1        leave the Compose project and the scratch files in place afterwards
#   HTTPS_PROXY, NO_PROXY and PROXY_CA_CERT_B64 are passed to the steps that
#   download modules; the Compose network needs no proxy.
set -euo pipefail

readonly DEFAULT_HEARSAY_REF=44082baf6c736d511e4ff3cc14751b1455c039d9
hearsay_ref=${HEARSAY_REF:-$DEFAULT_HEARSAY_REF}

[[ -f go.mod && -d internal/service && -d deploy/hearsay-smoke ]] || { echo "run this from the Osmia repository root" >&2; exit 2; }
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "docker compose is required" >&2; exit 1; }
root=$(pwd)

work=$(mktemp -d "${TMPDIR:-/tmp}/osmia-hearsay-smoke.XXXXXXXX")
evidence=${1:-$work/evidence}
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
stack=$work/stack
project=osmia-hearsay-smoke
image=osmia-hearsay-smoke:${hearsay_ref:0:12}
mkdir -p "$stack"
cp -R deploy/hearsay-smoke/. "$stack/"

proxy_args=()
for v in HTTPS_PROXY HTTP_PROXY https_proxy http_proxy NO_PROXY no_proxy; do
    if [[ -n "${!v:-}" ]]; then proxy_args+=(-e "$v"); fi
done
if [[ -n "${PROXY_CA_CERT_B64:-}" ]]; then
    echo "$PROXY_CA_CERT_B64" | base64 -d >"$work/proxy-ca.crt"
    proxy_args+=(-v "$work/proxy-ca.crt:/proxy-ca.crt:ro" -e SSL_CERT_FILE=/proxy-ca.crt)
fi

compose=(docker compose -p "$project" -f "$stack/compose.yaml" --env-file "$stack/.env")
cleanup() {
    local status=$?
    trap - EXIT
    if [[ -n "${KEEP:-}" ]]; then
        echo "KEEP is set: Compose project $project is still running; scratch files are in $work"
    else
        "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
        # The containers wrote some of these as root.
        docker run --rm -v "$work:/w" alpine:3.22 chmod -R a+rwX /w >/dev/null 2>&1 || true
        if [[ "$evidence" == "$work"/* ]]; then
            echo "evidence stays in $evidence; scratch files are in $work"
        else
            rm -rf "$work"
        fi
    fi
    exit "$status"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }

# ---- 1. Hearsay, built from the tree under test ----
step "Hearsay at $hearsay_ref"
if [[ -n "${HEARSAY_SRC:-}" ]]; then
    hearsay_src=$(cd "$HEARSAY_SRC" && pwd)
else
    hearsay_src=$work/hearsay-src
    mkdir -p "$hearsay_src"
    curl -fsSL "https://codeload.github.com/kpenfound/hearsay/tar.gz/$hearsay_ref" | tar xz -C "$hearsay_src" --strip-components=1
fi
hearsay_go=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "$hearsay_src/go.mod")
docker run --rm --network host "${proxy_args[@]}" \
    -v "$hearsay_src:/src:ro" -v "$stack:/out" -v osmia-smoke-gomod:/go/pkg/mod -v osmia-smoke-gobuild:/root/.cache/go-build \
    -e CGO_ENABLED=0 -e GOOS=linux -w /src "golang:$hearsay_go" \
    go build -trimpath -ldflags "-s -w" -o /out/hearsay ./cmd/hearsay
docker build -q -t "$image" "$stack" >/dev/null
echo "image $image: $(docker run --rm "$image" version)"

# ---- 2. Osmia's side, compiled once ----
osmia_go=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod)
step "Osmia smoke test binary (Go $osmia_go)"
mkdir -p "$work/bin"
docker run --rm --network host "${proxy_args[@]}" \
    -v "$root:/src:ro" -v "$work/bin:/out" -v osmia-smoke-gomod:/go/pkg/mod -v osmia-smoke-gobuild:/root/.cache/go-build \
    -w /src "golang:$osmia_go" \
    go test -c -tags hearsaysmoke -o /out/service.test ./internal/service

# run_osmia <docker args...> -- <test args...>
run_osmia() {
    local docker_args=()
    while [[ "$1" != "--" ]]; do docker_args+=("$1"); shift; done
    shift
    docker run --rm "${docker_args[@]}" -v "$work/bin:/bin-under-test:ro" -w /tmp "golang:$osmia_go" /bin-under-test/service.test "$@"
}

# ---- 3. The configuration `osmia project memory` generates ----
step "configuration from the memory setup"
mkdir -p "$stack/config"
run_osmia --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$stack/config:/config" -e OSMIA_SMOKE_CONFIG_DIR=/config -- -test.run '^TestHearsaySmokeConfig$' -test.v -test.count=1 | tee "$evidence/config-generation.log"
chmod -R a+rX "$stack/config"
mv "$stack/config/memory-setup-summary.json" "$evidence/"
cp -R "$stack/config" "$evidence/hearsay-config"

umask 077
hex() { openssl rand -hex 16; }
password=$(hex)
cat >"$stack/.env" <<ENV
HEARSAY_IMAGE=$image
POSTGRES_PASSWORD=$password
HEARSAY_DATABASE_URL=postgres://hearsay:$password@postgres:5432/hearsay?sslmode=disable
HEARSAY_SMOKE_MODEL_KEY=model-stub-key
ANTHROPIC_API_KEY=model-stub-key
HEARSAY_OWNER_TOKEN=$(hex)
HEARSAY_OSMIA_PUBLISHER_TOKEN=$(hex)
HEARSAY_OSMIA_WORKER_TOKEN=$(hex)
HEARSAY_OSMIA_ORCHESTRATOR_TOKEN=$(hex)
HEARSAY_OSMIA_OBSERVER_TOKEN=$(hex)
HEARSAY_SMOKE_OUTSIDER_TOKEN=$(hex)
ENV
chmod a+r "$stack/.env"

step "hearsay config validate"
docker run --rm --env-file "$stack/.env" -v "$stack/config:/etc/hearsay:ro" "$image" config validate /etc/hearsay | tee "$evidence/config-validate.txt"

# ---- 4. Hearsay under Compose ----
step "docker compose up"
if ! "${compose[@]}" up -d --wait --wait-timeout 240; then
    "${compose[@]}" ps -a || true
    "${compose[@]}" logs --no-color --tail 80 || true
    echo "Hearsay did not come up under Compose" >&2
    exit 1
fi
"${compose[@]}" ps -a | tee "$evidence/compose-ps.txt"

# ---- 5. The run ----
step "publisher pass and reads (container evidence)"
set -a
# shellcheck disable=SC1091
. "$stack/.env"
set +a
set +e
run_osmia --network "${project}_default" -v "$evidence:/evidence" \
    -e OSMIA_SMOKE_API_URL=http://api:8080 -e OSMIA_SMOKE_HOOKS_URL=http://connectors:8081 -e OSMIA_SMOKE_EVIDENCE=/evidence \
    -e HEARSAY_OWNER_TOKEN -e HEARSAY_OSMIA_PUBLISHER_TOKEN -e HEARSAY_OSMIA_WORKER_TOKEN -e HEARSAY_OSMIA_ORCHESTRATOR_TOKEN \
    -e HEARSAY_OSMIA_OBSERVER_TOKEN -e HEARSAY_SMOKE_OUTSIDER_TOKEN \
    -- -test.run '^TestHearsaySmokeRun$' -test.v -test.count=1 2>&1 | tee "$evidence/run.log"
status=${PIPESTATUS[0]}
set -e

# Decisions are placed from their structured fields with no model call: no
# decision text may have reached the model stub. (The sealed spec is a
# document and does go through the distill tier.)
"${compose[@]}" logs --no-color modelstub >"$evidence/modelstub.log" 2>&1 || true
if grep -q -e 'Treat the 10 MiB cap as inclusive' -e 'Should uploads be capped' "$evidence/modelstub.log"; then
    echo "FAIL: decision text reached the model stub" | tee -a "$evidence/run.log" >&2
    status=1
else
    echo "model stub: no decision text received" | tee -a "$evidence/run.log"
fi
"${compose[@]}" logs --no-color connectors >"$evidence/connectors.log" 2>&1 || true
"${compose[@]}" logs --no-color api >"$evidence/api.log" 2>&1 || true

step "result"
{
    echo "CONTAINER EVIDENCE: Hearsay $hearsay_ref built and run in containers under Docker Compose ($project);"
    echo "Osmia's publisher pass and read path ran in a container on the Compose network; the model provider is replaced by modelstub.py."
    echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "exit status of the run: $status"
} | tee "$evidence/RESULT.txt"
echo "evidence: $evidence"
exit "$status"
