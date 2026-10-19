#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=${1:-kvm}
case "$platform" in kvm|systrap) ;; *) echo "platform must be kvm or systrap" >&2; exit 1;; esac
test -x "$project_dir/bin/agentboxd"
test -x "$project_dir/bin/mockcore"
test -x "$project_dir/.local/gvisor/runsc"
image_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[0])' "$project_dir/.local/rootfs-image.json")
mkdir -p "$project_dir/artifacts"
result_dir=$(mktemp -d "$project_dir/artifacts/observe-$platform-XXXXXXXX")
echo "Observation artifacts: $result_dir"
device_args=()
if [[ "$platform" == kvm ]]; then device_args=(--device /dev/kvm); fi
# All cgroup writes belong to this explicitly isolated test controller. The
# observer inside agentboxd only reads its configured sources.
docker run --rm --init --privileged --cgroupns=private --network=none \
    --memory=5g --memory-swap=5g --cpus=2 --pids-limit=2304 \
    --tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777,size=268435456 \
    --tmpfs /state:rw,nosuid,nodev,mode=700,size=134217728 \
    "${device_args[@]}" -e "AGENTBOX_PLATFORM=$platform" \
    --mount "type=bind,source=$project_dir,target=/workspace,readonly" \
    --mount "type=bind,source=$result_dir,target=/results" \
    --entrypoint python3 "$image_ref" -u /workspace/tests/observation/container_validation.py
