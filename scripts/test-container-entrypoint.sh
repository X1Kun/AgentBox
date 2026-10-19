#!/usr/bin/env bash
set -euo pipefail

# Runs only inside an explicitly authorized, disposable test container with a
# PRIVATE cgroup namespace. Never bind mount the host cgroup tree here.
test -f /.dockerenv
test "$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup)" = /
test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
if [[ "${AGENTBOX_TEST_DEPENDENCIES_TMPFS:-0}" == 1 ]]; then
    echo 'Preparing test dependencies in container tmpfs (outside daemon startup budget)'
    cp -a /runtime-source/. /runtime/
    if [[ "${AGENTBOX_TEST_IMAGE_ROOTFS:-0}" != 1 ]]; then cp -a /rootfs-source/. /rootfs/; fi
    echo 'Test dependency preparation complete'
fi
if [[ "${AGENTBOX_TEST_IMAGE_ROOTFS:-0}" == 1 ]]; then
    # Materialize trusted image files, excluding all host test mounts and
    # live kernel filesystems. The OCI spec mounts this rootfs read-only.
    for directory in bin etc lib lib64 sbin usr var; do
        if [[ -e "/$directory" ]]; then cp -ax "/$directory" /rootfs/; fi
    done
    mkdir -p /rootfs/dev /rootfs/proc /rootfs/sys /rootfs/tmp /rootfs/workspace /rootfs/agentbox
    chmod 0755 /rootfs /rootfs/dev /rootfs/proc /rootfs/sys /rootfs/workspace /rootfs/agentbox
    chmod 1777 /rootfs/tmp
    echo 'Test rootfs prepared from the pinned Python image'
fi
mkdir /sys/fs/cgroup/test-manager
echo "$$" > /sys/fs/cgroup/test-manager/cgroup.procs
if [[ "${AGENTBOX_QOS_TEST:-0}" == 1 ]]; then
    case "$(cat /proc/1/comm)" in docker-init|tini) ;; *) echo 'requires Docker --init' >&2; exit 1;; esac
    echo 1 > /sys/fs/cgroup/test-manager/cgroup.procs
fi
echo '+cpu +memory +pids' > /sys/fs/cgroup/cgroup.subtree_control
mkdir /sys/fs/cgroup/agentbox
export AGENTBOX_INTEGRATION=1
export AGENTBOX_RUNSC=/runtime/runsc
export AGENTBOX_ROOTFS=/rootfs
export AGENTBOX_CGROUP_ROOT=/sys/fs/cgroup/agentbox
export AGENTBOX_PLATFORM=${AGENTBOX_PLATFORM:-kvm}
# Match the supplied systemd service, including guest-input creation permissions.
umask 0077
if [[ "${AGENTBOX_BENCHMARK:-0}" == 1 ]]; then
    exec /tests/benchmark.test -test.v -test.timeout="${AGENTBOX_BENCH_TEST_TIMEOUT:-20m}"
fi
if [[ "${AGENTBOX_QOS_TEST:-0}" == 1 ]]; then
    exec /tests/integration.test -test.v -test.run='^Test(CPU(ControlCgroup|QoSRuntime)|PressureSchedulingRuntime)$' -test.timeout=120s
fi
for mode in cold warm; do
    echo "Running real runtime suite: $AGENTBOX_PLATFORM / $mode"
    AGENTBOX_EXECUTION_MODE="$mode" /tests/integration.test -test.v -test.timeout=240s
done
