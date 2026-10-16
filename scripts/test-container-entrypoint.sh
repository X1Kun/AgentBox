#!/usr/bin/env bash
set -euo pipefail

# Runs only inside an explicitly authorized, disposable test container with a
# PRIVATE cgroup namespace. Never bind mount the host cgroup tree here.
test -f /.dockerenv
test "$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup)" = /
test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
mkdir /sys/fs/cgroup/test-manager
echo "$$" > /sys/fs/cgroup/test-manager/cgroup.procs
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
for mode in cold warm; do
    echo "Running real runtime suite: $AGENTBOX_PLATFORM / $mode"
    AGENTBOX_EXECUTION_MODE="$mode" /tests/integration.test -test.v -test.timeout=240s
done
