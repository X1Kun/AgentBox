#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=${1:-kvm}
samples=${2:-60}
rate=${3:-10}
case "$platform" in kvm|systrap) ;; *) echo "platform must be kvm or systrap" >&2; exit 1;; esac
[[ "$samples" =~ ^[0-9]+$ && "$rate" =~ ^[0-9]+$ ]] || { echo "samples and rate must be integers" >&2; exit 1; }
(( samples >= 10 && samples <= 1000 && rate >= 1 && rate <= 100 )) || { echo "samples must be 10..1000; rate must be 1..100" >&2; exit 1; }
test -x "$project_dir/.local/gvisor/runsc"
test -e "$project_dir/.local/rootfs/usr/local/bin/python3"
image_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[0])' "$project_dir/.local/rootfs-image.json")
mkdir -p "$project_dir/bin" "$project_dir/artifacts"
(cd "$project_dir" && CGO_ENABLED=0 go test -tags=integration -c -o bin/benchmark.test ./tests/benchmarks)
test_timeout=$((1200 + (2*samples + rate - 1)/rate))s
result_dir=$(mktemp -d "$project_dir/artifacts/benchmark-$platform-XXXXXXXX")
echo "Benchmark artifacts: $result_dir"
device_args=()
if [[ "$platform" == kvm ]]; then device_args=(--device /dev/kvm); fi
docker run --rm --privileged --cgroupns=private --network=none \
    --memory=5g --memory-swap=5g --cpus=6 --pids-limit=2304 \
    "${device_args[@]}" \
    -e "AGENTBOX_PLATFORM=$platform" -e AGENTBOX_BENCHMARK=1 \
    -e "AGENTBOX_BENCH_SAMPLES=$samples" -e "AGENTBOX_BENCH_RATE=$rate" \
    -e "AGENTBOX_BENCH_TEST_TIMEOUT=$test_timeout" -e AGENTBOX_BENCH_OUTPUT=/results -e AGENTBOX_BENCH_ENVIRONMENT=isolated-docker \
    -e "AGENTBOX_ROOTFS_IMAGE=$image_ref" \
    --mount "type=bind,source=$project_dir/.local/gvisor,target=/runtime,readonly" \
    --mount "type=bind,source=$project_dir/.local/rootfs,target=/rootfs,readonly" \
    --mount "type=bind,source=$project_dir/bin,target=/tests,readonly" \
    --mount "type=bind,source=$project_dir/scripts/test-container-entrypoint.sh,target=/entrypoint.sh,readonly" \
    --mount "type=bind,source=$result_dir,target=/results" \
    --entrypoint /bin/bash "$image_ref" /entrypoint.sh
