#!/bin/sh
# Read-only process evidence inside an owned kind node. Args: sandbox ID.
set -eu
sandbox_id=$1
case "$sandbox_id" in *[!a-f0-9]*|'') exit 2;; esac
for proc in /proc/[0-9]*; do
    exe=$(readlink "$proc/exe" 2>/dev/null) || continue
    case "$exe" in */runsc|*/gvisor_sentry|*/containerd-shim-runsc-v1) ;; *) continue;; esac
    args=$(tr '\000\n\t' '   ' < "$proc/cmdline" 2>/dev/null) || continue
    case "$args" in *"$sandbox_id"*) ;; *) continue;; esac
    pid=${proc#/proc/}
    # Remove pid/comm first; starttime is field 20 of the remaining stat fields.
    ticks=$(sed 's/^[^)]*) //' "$proc/stat" | awk '{print $20}')
    group=$(sed -n 's/^0:://p' "$proc/cgroup")
    printf '%s\t%s\t%s\t%s\t%s\n' "$pid" "$ticks" "$group" "$exe" "$args"
done
