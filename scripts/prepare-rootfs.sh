#!/usr/bin/env bash
set -euo pipefail

# Export a trusted image without starting it. The resolved digest is recorded.
# For repeatability, rerun with the recorded python@sha256:... reference.
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
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
