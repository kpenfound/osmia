#!/usr/bin/env bash
set -euo pipefail

readonly TEMPLATE='osmia:latest'

if [[ $# -ne 0 ]]; then
    printf 'Usage: %s\n' "$0" >&2
    exit 2
fi
command -v sbx >/dev/null || { printf 'sbx is required on PATH.\n' >&2; exit 1; }
command -v dagger >/dev/null || { printf 'dagger is required on PATH; the template installs the host'"'"'s Dagger CLI release.\n' >&2; exit 1; }

host_dagger_version=$(dagger version | awk '$1 == "version:" {print $2}')
if [[ -z "$host_dagger_version" ]]; then
    printf 'Could not read the host Dagger version from: dagger version\n' >&2
    exit 1
fi
readonly DAGGER_VERSION="${host_dagger_version#v}"

# Runs as root in the build sandbox: stops any Docker Engine or containerd
# process, purges their packages and deletes what is left of the engine, the
# Docker CLI and its plugins, so a guest made from the template cannot start
# either daemon. Nothing here counts as evidence; the probe below decides.
read -r -d '' GUEST_REMOVAL <<'REMOVAL' || true
set -eu
is_engine() {
    case "$1" in
        dockerd | dockerd-* | containerd | containerd-* | docker-containerd*) return 0 ;;
    esac
    return 1
}
signal_engine() {
    for d in /proc/[0-9]*; do
        comm=$(cat "$d/comm" 2>/dev/null) || continue
        if is_engine "$comm"; then kill "-$1" "${d#/proc/}" 2>/dev/null || true; fi
    done
}
signal_engine TERM
sleep 2
signal_engine KILL

if command -v dpkg-query >/dev/null 2>&1; then
    pkgs=
    for p in docker-ce docker-ce-cli docker-ce-rootless-extras docker-buildx-plugin \
        docker-compose-plugin docker-model-plugin docker-buildx docker-compose docker-compose-v2 \
        docker-cli docker-doc docker.io containerd.io containerd; do
        if dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'ok installed'; then
            pkgs="$pkgs $p"
        fi
    done
    if [ -n "$pkgs" ]; then
        # shellcheck disable=SC2086
        DEBIAN_FRONTEND=noninteractive apt-get purge -y $pkgs
    fi
fi

for dir in /usr/local/sbin /usr/local/bin /usr/sbin /usr/bin /sbin /bin /opt/docker/bin; do
    for name in docker dockerd docker-init docker-proxy docker-compose containerd containerd-shim \
        containerd-shim-runc-v1 containerd-shim-runc-v2 containerd-stress ctr; do
        rm -f "${dir:?}/${name:?}"
    done
done
rm -rf /usr/libexec/docker /usr/lib/docker /usr/local/lib/docker /var/lib/docker /var/lib/containerd \
    /run/docker /run/containerd /var/run/docker /var/run/containerd
rm -f /run/docker.sock /var/run/docker.sock /etc/init.d/docker /etc/init.d/containerd \
    /etc/supervisor/conf.d/*docker* /etc/supervisor/conf.d/*containerd* \
    /lib/systemd/system/docker.* /lib/systemd/system/containerd.service \
    /etc/systemd/system/docker.* /etc/systemd/system/containerd.service
for home in /root /home/*; do
    rm -rf "${home:?}/.docker/cli-plugins" "${home:?}/.docker/contexts" "${home:?}/.docker/run"
done
REMOVAL

# Runs in the build sandbox as root and as the agent user. It prints one line
# per finding: "ok <probe>" for a clear absence, "found <probe>: <what>" for a
# clear presence and "unknown <probe>: <why>" when it cannot tell, then "done".
# Reachability is asked of the sockets and addresses themselves with curl, never
# of a Docker CLI, whose absence proves nothing. PROBE_ROOT prefixes every
# filesystem path and PROBE_PROC replaces /proc; both are unset in a real guest.
read -r -d '' GUEST_PROBE <<'PROBE' || true
root=${PROBE_ROOT:-}
proc=${PROBE_PROC:-/proc}
say() { printf '%s\n' "$*"; }
engine_name() {
    case "$1" in
        dockerd | dockerd-* | containerd | containerd-* | docker-containerd*) return 0 ;;
    esac
    return 1
}

probe_processes() {
    local d comm arg0 seen=0 hits=
    if [ ! -d "$proc" ] || [ ! -r "$proc" ]; then
        say "unknown processes: $proc is not readable"
        return
    fi
    for d in "$proc"/[0-9]*; do
        comm=$(cat "$d/comm" 2>/dev/null) || continue
        seen=$((seen + 1))
        arg0=$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null | head -n 1)
        arg0=${arg0##*/}
        if engine_name "$comm" || engine_name "$arg0"; then
            hits="$hits ${d##*/}:${comm:-$arg0}"
        fi
    done
    if [ "$seen" -eq 0 ]; then
        say "unknown processes: no process was visible under $proc"
    elif [ -n "$hits" ]; then
        say "found processes: running Docker Engine or containerd processes (pid:name)$hits"
    else
        say "ok processes"
    fi
}

probe_binaries() {
    local dirs d n hits= ifs=$IFS
    dirs="/usr/local/sbin /usr/local/bin /usr/sbin /usr/bin /sbin /bin"
    IFS=:
    for d in $PATH; do
        if [ -n "$d" ]; then dirs="$dirs $d"; fi
    done
    IFS=$ifs
    for d in $dirs; do
        for n in docker dockerd docker-init docker-proxy containerd containerd-shim containerd-shim-runc-v2; do
            if [ -e "$root$d/$n" ] || [ -L "$root$d/$n" ]; then hits="$hits $d/$n"; fi
        done
    done
    if [ -n "$hits" ]; then
        say "found binaries: Docker or containerd executables remain:$hits"
    else
        say "ok binaries"
    fi
}

# ask <kind> <label> <curl args...> classifies the answer of one endpoint. Only
# a connection that cannot be made (curl exit 7) or a name that does not resolve
# (exit 6) is a clear absence; any other failure is unknown.
reported_endpoint=0
ask() {
    local kind=$1 label=$2 out rc body code
    shift 2
    if ! command -v curl >/dev/null 2>&1; then
        say "unknown endpoints: $label exists but curl is not installed to ask it"
        reported_endpoint=1
        return
    fi
    out=$(curl -sS --noproxy '*' --max-time 5 -w '\n%{http_code}' "$@" 2>&1)
    rc=$?
    if [ "$rc" -eq 0 ]; then
        code=${out##*$'\n'}
        body=${out%$'\n'*}
        if [ "$kind" = docker ] && [ "$code" = 200 ] && [ "$body" = OK ]; then
            say "found endpoints: a Docker daemon answered /_ping at $label"
        elif [ "$kind" = containerd ]; then
            say "found endpoints: something answered at containerd socket $label"
        else
            say "unknown endpoints: $label answered HTTP $code, which is not a Docker /_ping reply"
        fi
        reported_endpoint=1
        return
    fi
    case $rc in
        6 | 7) return ;;
    esac
    # containerd speaks gRPC, so a failure other than a timeout still shows a
    # listener.
    if [ "$kind" = containerd ] && [ "$rc" -ne 28 ]; then
        say "found endpoints: something is listening on containerd socket $label"
        reported_endpoint=1
        return
    fi
    say "unknown endpoints: could not tell whether $label is a daemon (curl exit $rc: ${out//$'\n'/ })"
    reported_endpoint=1
}

ask_unix() {
    local kind=$1 path=$2
    if [ ! -e "$root$path" ] && [ ! -L "$root$path" ]; then return; fi
    if [ ! -S "$root$path" ]; then
        say "unknown endpoints: $path exists but is not a socket"
        reported_endpoint=1
        return
    fi
    if [ ! -w "$root$path" ]; then
        say "unknown endpoints: $path exists but this user may not connect to it"
        reported_endpoint=1
        return
    fi
    ask "$kind" "unix socket $path" --unix-socket "$root$path" http://localhost/_ping
}

ask_host() {
    local host=$1 source=$2 rest
    case "$host" in
        unix://*) ask_unix docker "${host#unix://}" ;;
        tcp://* | http://*)
            rest=${host#*://}
            rest=${rest%%/*}
            ask docker "$source $host" "http://$rest/_ping" ;;
        *)
            say "unknown endpoints: $source $host has a scheme this probe cannot ask"
            reported_endpoint=1 ;;
    esac
}

probe_endpoints() {
    local home=${HOME:-} xdg=${XDG_RUNTIME_DIR:-} p f host
    if ! command -v find >/dev/null 2>&1; then
        say "unknown endpoints: find is not installed to look for forwarded sockets"
        return
    fi
    if [ -n "${DOCKER_HOST:-}" ]; then ask_host "$DOCKER_HOST" "DOCKER_HOST"; fi
    for f in "$root$home"/.docker/contexts/meta/*/meta.json; do
        [ -e "$f" ] || continue
        for host in $(grep -o '"Host" *: *"[^"]*"' "$f" | sed 's/.*: *"\(.*\)"/\1/'); do
            ask_host "$host" "Docker context $f"
        done
    done
    for p in /var/run/docker.sock /run/docker.sock ${xdg:+"$xdg/docker.sock"} "$home/.docker/run/docker.sock"; do
        ask_unix docker "$p"
    done
    ask_unix containerd /run/containerd/containerd.sock
    ask_unix containerd /var/run/containerd/containerd.sock
    ask docker "tcp 127.0.0.1:2375" http://127.0.0.1:2375/_ping
    # Forwarded or relocated sockets: any socket named for docker or containerd.
    for f in $(find "$root/run" "$root/var/run" "$root/tmp" ${home:+"$root$home"} -maxdepth 4 -type s \
        \( -name '*docker*' -o -name '*containerd*' \) 2>/dev/null); do
        p=${f#"$root"}
        case "$p" in
            /var/run/docker.sock | /run/docker.sock | "$xdg/docker.sock" | "$home/.docker/run/docker.sock" | /run/containerd/containerd.sock | /var/run/containerd/containerd.sock) continue ;;
            *containerd*) ask_unix containerd "$p" ;;
            *) ask_unix docker "$p" ;;
        esac
    done
    if [ "$reported_endpoint" -eq 0 ]; then say "ok endpoints"; fi
}

probe_processes
probe_binaries
probe_endpoints
say done
PROBE

# prove_docker_free <label> [sbx exec options...] runs the guest probe and
# appends every refusal reason to problems. A probe that fails to run, prints
# something unexpected or leaves a required probe unanswered is a refusal.
problems=()
prove_docker_free() {
    local label=$1 out line status name seen=' ' have_done=0
    shift
    if ! out=$(sbx exec -i "$@" "$sandbox" bash -s osmia-docker-probe <<<"$GUEST_PROBE"); then
        problems+=("as $label: the probe could not run (sbx exec failed)")
        return
    fi
    while IFS= read -r line; do
        status=${line%% *}
        name=${line#* }
        name=${name%%:*}
        case "$status $name" in
            "ok processes" | "ok binaries" | "ok endpoints") seen="$seen$name " ;;
            "found processes" | "found binaries" | "found endpoints" | "unknown processes" | "unknown binaries" | "unknown endpoints")
                seen="$seen$name "
                problems+=("as $label: $line") ;;
            "done done") have_done=1 ;;
            *) problems+=("as $label: unexpected probe output: $line") ;;
        esac
    done <<<"$out"
    for name in processes binaries endpoints; do
        case "$seen" in
            *" $name "*) ;;
            *) problems+=("as $label: the $name probe gave no answer") ;;
        esac
    done
    if [[ $have_done -ne 1 ]]; then
        problems+=("as $label: the probe did not run to completion")
    fi
}

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
# The codex template can carry Docker Engine and the Docker CLI. The template
# keeps neither, so they go before anything is installed.
sbx exec -i -u root "$sandbox" sh -s osmia-docker-removal <<<"$GUEST_REMOVAL"
# The codex template ships Codex; Claude Code comes from its native installer.
sbx exec -i "$sandbox" bash -eu <<'CLAUDE'
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
curl -fsSL https://downloads.claude.ai/claude-code-releases/bootstrap.sh -o "$installer"
bash "$installer"
CLAUDE
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

# Both the root and the agent user are probed: DOCKER_HOST and socket access
# differ between them.
prove_docker_free root -u root
prove_docker_free agent
if [[ ${#problems[@]} -gt 0 ]]; then
    printf 'Refusing to save template %s; it must have no Docker Engine:\n' "$TEMPLATE" >&2
    printf '  %s\n' "${problems[@]}" >&2
    exit 1
fi
printf 'No Docker Engine in the build sandbox: no dockerd or containerd process and no reachable daemon.\n'

sbx stop "$sandbox"
sbx template save "$sandbox" "$TEMPLATE"
printf 'Saved local template %s with Dagger v%s.\n' "$TEMPLATE" "$DAGGER_VERSION"
