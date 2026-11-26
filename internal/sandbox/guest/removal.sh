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
# sbx attaches a data disk at /var/lib/docker. A mount point cannot be removed,
# so it is emptied and left in place.
is_mount_point() {
    awk -v p="$1" '$5 == p { found = 1 } END { exit !found }' /proc/self/mountinfo
}
for path in /usr/libexec/docker /usr/lib/docker /usr/local/lib/docker /var/lib/docker /var/lib/containerd \
    /run/docker /run/containerd /var/run/docker /var/run/containerd; do
    if is_mount_point "$path"; then
        find "$path" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
    else
        rm -rf "$path"
    fi
done
rm -f /run/docker.sock /var/run/docker.sock /etc/init.d/docker /etc/init.d/containerd \
    /etc/supervisor/conf.d/*docker* /etc/supervisor/conf.d/*containerd* \
    /lib/systemd/system/docker.* /lib/systemd/system/containerd.service \
    /etc/systemd/system/docker.* /etc/systemd/system/containerd.service
for home in /root /home/*; do
    rm -rf "${home:?}/.docker/cli-plugins" "${home:?}/.docker/contexts" "${home:?}/.docker/run"
done
