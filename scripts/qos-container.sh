#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=${1:-kvm}
test_pattern=${2:-'^Test(CPU(ControlCgroup|QoSRuntime)|PressureSchedulingRuntime)$'}
case "$platform" in kvm|systrap) ;; *) echo 'platform must be kvm or systrap' >&2; exit 1;; esac
image_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[0])' "$project_dir/.local/rootfs-image.json")
(cd "$project_dir" && CGO_ENABLED=0 go test -tags=integration -c -o bin/integration.test ./tests/integration)
device_args=()
if [[ "$platform" == kvm ]]; then device_args=(--device /dev/kvm); fi
runtime_args=(--mount "type=bind,source=$project_dir/.local/gvisor,target=/runtime,readonly")
rootfs_args=(--mount "type=bind,source=$project_dir/.local/rootfs,target=/rootfs,readonly")
case "${AGENTBOX_TEST_DEPENDENCIES_TMPFS:-0}" in
    0) ;;
    1)
        runtime_args=(--mount "type=bind,source=$project_dir/.local/gvisor,target=/runtime-source,readonly" --tmpfs /runtime:rw,exec,nosuid,nodev,mode=755,size=536870912)
        rootfs_args=(--mount "type=bind,source=$project_dir/.local/rootfs,target=/rootfs-source,readonly" --tmpfs /rootfs:rw,exec,nosuid,nodev,mode=755,size=536870912)
        ;;
    *) echo 'AGENTBOX_TEST_DEPENDENCIES_TMPFS must be 0 or 1' >&2; exit 1;;
esac
case "${AGENTBOX_TEST_IMAGE_ROOTFS:-0}" in
    0) ;;
    1) rootfs_args=(--tmpfs /rootfs:rw,exec,nosuid,nodev,mode=755,size=536870912) ;;
    *) echo 'AGENTBOX_TEST_IMAGE_ROOTFS must be 0 or 1' >&2; exit 1;;
esac
docker run --rm --init --privileged --cgroupns=private --network=none \
    --memory=5g --memory-swap=5g --cpus=6 --pids-limit=2304 \
    --tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777,size=268435456 \
    "${device_args[@]}" "${runtime_args[@]}" "${rootfs_args[@]}" -e "AGENTBOX_PLATFORM=$platform" -e AGENTBOX_QOS_TEST=1 -e "AGENTBOX_TEST_RUN=$test_pattern" -e "AGENTBOX_TEST_DEPENDENCIES_TMPFS=${AGENTBOX_TEST_DEPENDENCIES_TMPFS:-0}" -e "AGENTBOX_TEST_IMAGE_ROOTFS=${AGENTBOX_TEST_IMAGE_ROOTFS:-0}" \
    --mount "type=bind,source=$project_dir/bin,target=/tests,readonly" \
    --mount "type=bind,source=$project_dir/scripts/test-container-entrypoint.sh,target=/entrypoint.sh,readonly" \
    --entrypoint /bin/bash "$image_ref" /entrypoint.sh
