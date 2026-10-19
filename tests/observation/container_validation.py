"""Observe real runsc and a separate core cgroup in a disposable container."""

import csv
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import time

sys.path.insert(0, "/workspace/scripts")
from observe import Client, record


def wait_until(predicate, description, timeout=30):
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except (OSError, ValueError):
            pass
        time.sleep(.05)
    raise RuntimeError("timed out waiting for "+description)


def limits(path):
    return {name: (path / name).read_text().strip() for name in ("cpu.max", "memory.high", "memory.max", "memory.swap.max", "pids.max")}


def main():
    root = Path("/sys/fs/cgroup")
    if not Path("/.dockerenv").exists() or Path("/proc/self/cgroup").read_text().strip() != "0::/":
        raise RuntimeError("requires disposable container with private cgroup namespace")
    if os.getpid()==1 or Path("/proc/1/comm").read_text().strip() not in ("docker-init","tini"):
        raise RuntimeError("requires Docker --init")
    manager = root / "observer-controller"
    manager.mkdir()
    (manager / "cgroup.procs").write_text("1")
    (manager / "cgroup.procs").write_text(str(os.getpid()))
    (root / "cgroup.subtree_control").write_text("+cpu +memory +pids")
    lab = root / "observation"
    lab.mkdir()
    (lab / "cgroup.subtree_control").write_text("+cpu +memory +pids")
    core_group, engine_group = lab / "core", lab / "agentbox"
    core_group.mkdir()
    engine_group.mkdir()
    (core_group / "memory.max").write_text(str(256 << 20))
    (core_group / "pids.max").write_text("64")
    token = secrets.token_hex(32)
    output = Path("/results")
    processes, logs = [], []

    def launch(arguments, group, name, environment):
        def join_group():
            (group / "cgroup.procs").write_text(str(os.getpid()))
        log = (output / (name+".log")).open("x")
        logs.append(log)
        process = subprocess.Popen(arguments, env=environment, stdout=log, stderr=subprocess.STDOUT, preexec_fn=join_group)
        processes.append(process)
        return process

    def stop(process):
        if process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.wait(timeout=40)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                raise RuntimeError("component exceeded graceful shutdown budget")
        if process.returncode != 0:
            raise RuntimeError("component failed; inspect its artifact log")

    platform = os.environ.get("AGENTBOX_PLATFORM", "kvm")
    environment = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    try:
        core = launch(["/workspace/bin/mockcore", "-iterations=50000", "-concurrency=4"], core_group, "mockcore", environment)
        daemon_env = dict(environment, AGENTBOX_API_TOKEN=token)
        daemon = launch(["/workspace/bin/agentboxd", "-runsc=/workspace/.local/gvisor/runsc", "-rootfs=/workspace/.local/rootfs",
                         "-state-dir=/state", "-cgroup-root=auto", "-platform="+platform, "-mode=warm", "-concurrency=2",
                         "-qos=observe", "-qos-interval=100ms", "-qos-protected-cgroup="+str(core_group)], engine_group, "agentbox", daemon_env)
        client, core_client = Client("http://127.0.0.1:8080", token), Client("http://127.0.0.1:8081")
        def observed():
            if daemon.poll() is not None:
                raise RuntimeError("daemon exited during startup; inspect agentbox.log")
            code, status = client.request("/v1/status")
            q = status.get("qos", {})
            return code == 200 and status["ready"] and status["idle"] == 3 and q.get("state") == "observing" and not q["stale"] and all(s["available"] and s["interval_ms"] is not None for s in q["sources"])
        wait_until(lambda: core_client.request("/healthz")[0] == 200, "core readiness")
        wait_until(observed, "valid observation sources and warm inventory")
        _, initial_status = client.request("/v1/status")
        (output/"initial-status.json").write_text(json.dumps(initial_status, indent=2)+"\n")
        sources = initial_status["qos"]["sources"]
        if not any(s["name"]=="manager" for s in sources) or not any(s["name"].startswith("ancestor_") for s in sources):
            raise RuntimeError("management or ancestor scope missing")
        sandbox_parent = engine_group / "sandboxes"
        before = {"sandboxes": limits(sandbox_parent), "protected": limits(core_group)}
        report = record("http://127.0.0.1:8081", "http://127.0.0.1:8080", token, output, duration=5, rate=20, sandbox_workers=2)
        after = {"sandboxes": limits(sandbox_parent), "protected": limits(core_group)}
        if before != after:
            raise RuntimeError("observation changed resource controls")
        for phase in report["phases"]:
            if phase["core_errors"] or phase["observation_errors"] or phase["sandbox_errors"] or phase["core_succeeded"] == 0:
                raise RuntimeError("observation experiment had transport errors")
        if len(report["phases"][0]["workload_checksums"]) != 1 or report["phases"][0]["workload_checksums"] != report["phases"][1]["workload_checksums"]:
            raise RuntimeError("core workload was not consistent between phases")
        if report["phases"][1]["sandbox_completed_timeouts"] == 0:
            raise RuntimeError("sandbox load did not execute")
        with (output/"sandbox-load-sandboxes.csv").open() as stream:
            tasks = list(csv.DictReader(stream))
        successful_responses = [row for row in tasks if row["http_status"]=="200"]
        expected_phases = ["runtime_delete", "cgroup_kill", "cgroup_empty_and_remove", "output_readers", "bundle_remove"]
        if not successful_responses or any([p["phase"] for p in json.loads(row.get("cleanup_phases_json") or "[]")] != expected_phases for row in successful_responses):
            raise RuntimeError("task cleanup phases missing or reordered")
        _, final_status = client.request("/v1/status")
        (output/"post-load-status.json").write_text(json.dumps(final_status, indent=2)+"\n")
        scheduling = final_status["scheduling"]
        creations = sum(scheduling["creation_"+outcome]["count"] for outcome in ("succeeded", "failed", "aborted"))
        if scheduling["creation_attempts"] != creations+final_status["creating"]:
            raise RuntimeError("creation accounting does not conserve attempts")
        outcomes = sum(scheduling["queue_"+outcome]["count"] for outcome in ("assigned", "canceled", "timed_out", "unavailable"))
        if scheduling["queue_entered"] != outcomes+final_status["queued"]:
            raise RuntimeError("queue accounting does not conserve entrants")
        if final_status["qos"]["sequence"] <= initial_status["qos"]["sequence"]:
            raise RuntimeError("collector sequence did not advance")
        # Missing protected service is an observation error, not zero pressure
        # and not a reason for observe mode to stop accepting sandbox work.
        stop(core)
        core_group.rmdir()
        def missing():
            _, status = client.request("/v1/status")
            return status["ready"] and status["qos"]["state"] == "degraded" and any(s["name"] == "protected" and not s["available"] and s["sample"] is None for s in status["qos"]["sources"])
        wait_until(missing, "explicit missing protected source")
        core_group.mkdir()
        (core_group / "memory.max").write_text(str(256 << 20))
        (core_group / "pids.max").write_text("64")
        core = launch(["/workspace/bin/mockcore", "-iterations=50000", "-concurrency=4"], core_group, "mockcore-restarted", environment)
        wait_until(lambda: core_client.request("/healthz")[0] == 200, "restarted core readiness")
        wait_until(observed, "observation recovery")
        stop(daemon)
        if any(Path("/state/bundles").iterdir()) or any(p.is_dir() for p in sandbox_parent.iterdir()):
            raise RuntimeError("daemon left sandbox resources")
        stop(core)
        evidence = {"platform": platform, "kernel": os.uname().release, "qos_mode": "observe", "limits_before": before, "limits_after": after,
                    "outer_limits": {name: (root / name).read_text().strip() for name in ("cpu.max", "memory.max")},
                    "binary_sha256": hashlib.sha256(Path("/workspace/bin/agentboxd").read_bytes()).hexdigest(),
                    "mockcore_sha256": hashlib.sha256(Path("/workspace/bin/mockcore").read_bytes()).hexdigest(),
                    "checks": ["valid-sources", "manager-and-ancestors", "cleanup-phase-traces", "scheduling-accounting", "sample-sequence", "core-load-recorded", "sandbox-load-executed", "limits-unchanged", "missing-source-not-zero", "source-recreated", "clean-shutdown"]}
        (output / "validation.json").write_text(json.dumps(evidence, indent=2)+"\n")
        print(json.dumps(evidence, indent=2))
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=40)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        for log in logs:
            log.close()
        # Reap adopted runtime children after all directly managed processes.
        try:
            while os.waitpid(-1, os.WNOHANG)[0]:
                pass
        except ChildProcessError:
            pass
        for path in output.iterdir():
            if path.is_file():
                path.chmod(0o644)


if __name__ == "__main__":
    main()
