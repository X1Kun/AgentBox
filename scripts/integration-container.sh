#!/usr/bin/env bash
set -euo pipefail

project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=${1:-kvm}
case "$platform" in kvm|systrap) ;; *) echo "platform must be kvm or systrap" >&2; exit 1;; esac
test -x "$project_dir/.local/gvisor/runsc"
test -e "$project_dir/.local/rootfs/usr/local/bin/python3"
image_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[0])' "$project_dir/.local/rootfs-image.json")
mkdir -p "$project_dir/bin"
(cd "$project_dir" && CGO_ENABLED=0 go test -tags=integration -c -o bin/integration.test ./tests/integration)
device_args=()
if [[ "$platform" == kvm ]]; then device_args=(--device /dev/kvm); fi
# Privilege is confined to a disposable Docker workload. No host /sys mount,
# host PID/network namespace, Docker socket, or writable source tree is passed.
docker run --rm --privileged --cgroupns=private --network=none \
    --memory=5g --memory-swap=5g --cpus=6 --pids-limit=2304 \
    "${device_args[@]}" \
    -e "AGENTBOX_PLATFORM=$platform" \
    --mount "type=bind,source=$project_dir/.local/gvisor,target=/runtime,readonly" \
    --mount "type=bind,source=$project_dir/.local/rootfs,target=/rootfs,readonly" \
    --mount "type=bind,source=$project_dir/bin,target=/tests,readonly" \
    --mount "type=bind,source=$project_dir/scripts/test-container-entrypoint.sh,target=/entrypoint.sh,readonly" \
    --entrypoint /bin/bash "$image_ref" /entrypoint.sh
