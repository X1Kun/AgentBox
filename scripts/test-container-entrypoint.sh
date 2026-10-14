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
exec /tests/integration.test -test.v -test.timeout=120s
