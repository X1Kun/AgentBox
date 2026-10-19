#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=${1:-kvm}
duration=${2:-60}
repetitions=${3:-3}
case "$platform" in kvm|systrap) ;; *) echo "platform must be kvm or systrap" >&2; exit 1;; esac
[[ "$duration" =~ ^[0-9]+$ && "$repetitions" =~ ^[0-9]+$ ]] || { echo "duration and repetitions must be integers" >&2; exit 1; }
(( duration >= 5 && duration <= 300 && repetitions >= 1 && repetitions <= 9 )) || { echo "duration must be 5..300; repetitions 1..9" >&2; exit 1; }
plot_python=${AGENTBOX_PLOT_PYTHON:-$project_dir/.local/plot-venv/bin/python}
if [[ ! -x "$plot_python" ]]; then plot_python=python3; fi
"$plot_python" -c 'import matplotlib' || { echo "Install scripts/plot-requirements.txt into .local/plot-venv or set AGENTBOX_PLOT_PYTHON" >&2; exit 1; }
test -x "$project_dir/bin/agentboxd"
test -x "$project_dir/bin/mockcore"
test -x "$project_dir/.local/gvisor/runsc"
image_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[0])' "$project_dir/.local/rootfs-image.json")
revision=$(git -C "$project_dir" rev-parse HEAD)
dirty=false
if [[ -n "$(git -C "$project_dir" status --porcelain)" ]]; then dirty=true; fi
mkdir -p "$project_dir/artifacts"
result_dir=$(mktemp -d "$project_dir/artifacts/static-$platform-XXXXXXXX")
echo "Static experiment artifacts: $result_dir"
device_args=()
if [[ "$platform" == kvm ]]; then device_args=(--device /dev/kvm); fi
docker run --rm --init --privileged --cgroupns=private --network=none \
    --memory=5g --memory-swap=5g --cpus=2 --pids-limit=2304 \
    --tmpfs /lab:rw,exec,nosuid,nodev,mode=755,size=1073741824 \
    --tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777,size=268435456 \
    "${device_args[@]}" -e "AGENTBOX_PLATFORM=$platform" -e PYTHONDONTWRITEBYTECODE=1 \
    -e "AGENTBOX_EXPERIMENT_DURATION=$duration" -e "AGENTBOX_EXPERIMENT_REPETITIONS=$repetitions" \
    -e "AGENTBOX_SOURCE_REVISION=$revision" -e "AGENTBOX_SOURCE_DIRTY=$dirty" \
    --mount "type=bind,source=$project_dir,target=/workspace,readonly" \
    --mount "type=bind,source=$result_dir,target=/results" \
    --entrypoint python3 "$image_ref" -u /workspace/tests/observation/static_container.py
"$plot_python" "$project_dir/scripts/plot-static.py" "$result_dir"
