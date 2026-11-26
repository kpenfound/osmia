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
