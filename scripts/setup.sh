#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

install_runsc() (
    release=${1:-20261005.0}
    arch=$(uname -m)
    case "$arch" in x86_64|aarch64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1;; esac
    if [[ ! "$release" =~ ^(latest|[0-9]{8}(\.[0-9]+)?)$ ]]; then
        echo "Release must be latest, YYYYMMDD, or YYYYMMDD.N" >&2
        exit 1
    fi
    destination="$project_dir/.local/gvisor"
    if [[ -e "$destination" ]]; then
        echo "Already exists: $destination; keeping the current installation" >&2
        exit 1
    fi
    mkdir -p "$project_dir/.local"
    temporary_dir=$(mktemp -d "$project_dir/.local/runsc-download.XXXXXXXX")
    trap 'rm -rf -- "$temporary_dir"' EXIT
    base_url="https://storage.googleapis.com/gvisor/releases/release/$release/$arch"
    curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
        "$base_url/gvisor.tar.bz2" -o "$temporary_dir/gvisor.tar.bz2"
    curl --fail --location --retry 2 --connect-timeout 15 --max-time 60 \
        "$base_url/gvisor.tar.bz2.sha512" -o "$temporary_dir/gvisor.tar.bz2.sha512"
    (cd "$temporary_dir" && sha512sum --check gvisor.tar.bz2.sha512)
    mkdir "$temporary_dir/install"
    tar -xjf "$temporary_dir/gvisor.tar.bz2" -C "$temporary_dir/install" --no-same-owner
    test -x "$temporary_dir/install/runsc"
    "$temporary_dir/install/runsc" --version > "$temporary_dir/install/VERSION.txt"
    cp "$temporary_dir/gvisor.tar.bz2.sha512" "$temporary_dir/install/ARCHIVE.sha512"
    printf '%s\n' "$base_url" > "$temporary_dir/install/SOURCE.txt"
    mv "$temporary_dir/install" "$destination"
    cat "$destination/VERSION.txt"
)

prepare_rootfs() (
    image=${1:-python:3.12-slim-bookworm}
    destination="$project_dir/.local/rootfs"
    if [[ -e "$destination" ]]; then
        echo "Already exists: $destination; keeping the current rootfs" >&2
        exit 1
    fi
    mkdir -p "$project_dir/.local"
    temporary_dir=$(mktemp -d "$project_dir/.local/rootfs-export.XXXXXXXX")
    container_id=
    cleanup() {
        if [[ -n "$container_id" ]]; then docker container rm "$container_id" >/dev/null; fi
        rm -rf -- "$temporary_dir"
    }
    trap cleanup EXIT
    docker pull "$image"
    docker image inspect "$image" --format '{{json .RepoDigests}}' > "$temporary_dir/rootfs-image.json"
    container_id=$(docker create "$image" /bin/true)
    docker export "$container_id" -o "$temporary_dir/rootfs.tar"
    mkdir "$temporary_dir/rootfs"
    tar -xf "$temporary_dir/rootfs.tar" -C "$temporary_dir/rootfs" --no-same-owner --same-permissions
    mkdir -p "$temporary_dir/rootfs/agentbox" "$temporary_dir/rootfs/workspace"
    # Guest UID 1000 must traverse the exposed root and mountpoints regardless of
    # the caller's umask or an archive entry for the root directory itself.
    chmod 0755 "$temporary_dir/rootfs" "$temporary_dir/rootfs/agentbox" "$temporary_dir/rootfs/workspace"
    test -e "$temporary_dir/rootfs/usr/local/bin/python3"
    mv "$temporary_dir/rootfs" "$destination"
    mv "$temporary_dir/rootfs-image.json" "$project_dir/.local/rootfs-image.json"
    cat "$project_dir/.local/rootfs-image.json"
)

command=${1:-help}
if (( $# )); then shift; fi
case "$command" in
    runsc) install_runsc "$@" ;;
    rootfs) prepare_rootfs "$@" ;;
    help|-h|--help) echo 'Usage: bash scripts/setup.sh runsc [release] | rootfs [image]' ;;
    *) echo 'Unknown setup command' >&2; exit 2 ;;
esac
