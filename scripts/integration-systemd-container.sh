#!/usr/bin/env bash
set -euo pipefail

project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=${1:-kvm}
case "$platform" in kvm|systrap) ;; *) echo "platform must be kvm or systrap" >&2; exit 1;; esac
test -x "$project_dir/bin/agentboxd"
test -x "$project_dir/.local/gvisor/runsc"
test -e "$project_dir/.local/rootfs/usr/local/bin/python3"
# Use an already available image by immutable local ID; do not download or
# reuse any running kind node. The container has its own PID/cgroup namespaces.
image_id=$(docker image inspect "${AGENTBOX_SYSTEMD_TEST_IMAGE:-kindest/node:v1.29.2}" --format '{{.Id}}')
container_id=
cleanup() {
    status=$?
    if [[ -n "$container_id" ]]; then
        if [[ "$status" != 0 ]]; then
            docker logs --tail 60 "$container_id" >&2 || true
            docker exec "$container_id" journalctl -u agentbox.service -n 60 --no-pager >&2 || true
        fi
        docker rm -f "$container_id" >/dev/null
    fi
}
trap cleanup EXIT
device_args=()
if [[ "$platform" == kvm ]]; then device_args=(--device /dev/kvm); fi
container_id=$(docker run --detach --privileged --cgroupns=private --network=none \
    --memory=5g --memory-swap=5g --cpus=6 --pids-limit=2304 \
    --tmpfs /run:rw,nosuid,nodev,mode=755 \
    --stop-signal SIGRTMIN+3 \
    "${device_args[@]}" -e "AGENTBOX_PLATFORM=$platform" \
    --mount "type=bind,source=$project_dir,target=/workspace,readonly" \
    --entrypoint /sbin/init "$image_id" --unit=basic.target)
ready=false
for _ in {1..100}; do
    if docker exec "$container_id" systemctl show-environment >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 0.2
done
if [[ "$ready" != true ]]; then echo "systemd did not become available" >&2; exit 1; fi
docker exec -e PYTHONDONTWRITEBYTECODE=1 "$container_id" python3 -u /workspace/tests/systemd/container_validation.py
