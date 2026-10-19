"""Static CPU experiments; requires an explicitly isolated Docker container."""

from datetime import datetime, timezone
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import sys
import time

sys.path.insert(0, "/workspace/scripts")
from observe import Client, run_phase, sandbox_workload
from static_experiment import check_phase, experiment_plan, write_json


def wait_until(predicate, description, timeout=40):
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except (OSError, ValueError):
            pass
        time.sleep(.1)
    raise RuntimeError("timed out waiting for "+description)


def limits(group):
    return {name: (group/name).read_text().strip()
            for name in ("cpu.max", "memory.high", "memory.max", "memory.swap.max", "pids.max")}


def main():
    root, output = Path("/sys/fs/cgroup"), Path("/lab/results")
    if not Path("/.dockerenv").exists() or Path("/proc/self/cgroup").read_text().strip() != "0::/":
        raise RuntimeError("requires disposable Docker container with private cgroup namespace")
    supervisor = Path("/proc/1/comm").read_text().strip()
    if os.getpid() == 1 or supervisor not in ("docker-init", "tini"):
        raise RuntimeError("requires Docker --init to continuously reap orphaned runtime children")
    duration = int(os.environ.get("AGENTBOX_EXPERIMENT_DURATION", "60"))
    repetitions = int(os.environ.get("AGENTBOX_EXPERIMENT_REPETITIONS", "3"))
    if not 5 <= duration <= 300:
        raise ValueError("duration must be 5..300s")
    preparation_started = time.monotonic()
    print("Preparing runsc, rootfs, binaries and measurements on container tmpfs", flush=True)
    output.mkdir()
    shutil.copytree("/workspace/.local/gvisor", "/lab/gvisor", symlinks=True,
                    ignore=shutil.ignore_patterns("containerd-shim-runsc-v1"))
    shutil.copytree("/workspace/.local/rootfs", "/lab/rootfs", symlinks=True)
    Path("/lab/bin").mkdir()
    for binary in ("agentboxd", "mockcore"):
        shutil.copy2("/workspace/bin/"+binary, "/lab/bin/"+binary)
    runtime_wrapper = Path("/lab/runsc-observed")
    runtime_wrapper.write_text('#!/bin/sh\nexec /lab/gvisor/runsc --debug --debug-command=delete '
                               '--debug-log=/lab/results/runsc-debug.log --debug-log-format=json "$@"\n')
    runtime_wrapper.chmod(0o755)
    preparation_seconds = time.monotonic()-preparation_started
    print(f"Memory snapshot ready after {preparation_seconds:.1f}s (outside measurement)", flush=True)
    plan = experiment_plan(repetitions)
    platform = os.environ.get("AGENTBOX_PLATFORM", "kvm")
    rate, edge_seconds = 20, 5
    manager = root / "experiment-controller"
    manager.mkdir()
    # Both the init reaper and controller must leave the namespace root before
    # enabling controllers (cgroup v2's no-internal-process rule).
    (manager/"cgroup.procs").write_text("1")
    (manager/"cgroup.procs").write_text(str(os.getpid()))
    (root/"cgroup.subtree_control").write_text("+cpu +memory +pids")
    lab = root/"static-experiment"
    lab.mkdir()
    (lab/"cgroup.subtree_control").write_text("+cpu +memory +pids")
    core_group = lab/"core"
    core_group.mkdir()
    (core_group/"memory.max").write_text(str(256 << 20))
    (core_group/"memory.swap.max").write_text("0")
    (core_group/"pids.max").write_text("64")
    token = secrets.token_hex(32)
    processes, logs = [], []

    def launch(arguments, group, name, environment):
        def join_group():
            (group/"cgroup.procs").write_text(str(os.getpid()))
        log = (output/(name+".log")).open("x")
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
                raise RuntimeError("component exceeded shutdown budget")
        if process.returncode != 0:
            raise RuntimeError("component failed; inspect its log")

    environment = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    workload = sandbox_workload("fixed")
    report = {"format": 1, "recorded_at": datetime.now(timezone.utc).isoformat(), "platform": platform,
              "kernel": os.uname().release, "supervisor": supervisor,
              "runsc_version": subprocess.check_output(["/lab/gvisor/runsc", "--version"], text=True).strip(),
              "rootfs_images": json.loads(Path("/workspace/.local/rootfs-image.json").read_text()),
              "source_revision": os.environ.get("AGENTBOX_SOURCE_REVISION", "unknown"),
              "source_dirty": os.environ.get("AGENTBOX_SOURCE_DIRTY") == "true",
              "measurement_sha256": {path: hashlib.sha256((Path("/workspace")/path).read_bytes()).hexdigest()
                                     for path in ("scripts/observe.py", "scripts/static_experiment.py", "tests/observation/static_container.py")},
              "binary_sha256": hashlib.sha256(Path("/lab/bin/agentboxd").read_bytes()).hexdigest(),
              "mockcore_sha256": hashlib.sha256(Path("/lab/bin/mockcore").read_bytes()).hexdigest(),
              "runsc_sha256": hashlib.sha256(Path("/lab/gvisor/runsc").read_bytes()).hexdigest(),
              "host_io_pressure_at_setup": Path("/proc/pressure/io").read_text().strip(),
              "tmpfs_used_bytes_at_setup": shutil.disk_usage("/lab").used,
              "snapshot_preparation_seconds": preparation_seconds,
              "runtime_debug_command": "delete",
              "outer_limits": limits(root), "core_limits": limits(core_group),
              "config": {"duration_seconds": duration, "repetitions": repetitions, "edge_seconds": edge_seconds,
                         "core_rate": rate, "core_iterations": 50000, "core_concurrency": 4,
                         "sandbox_workers": 2, "sandbox_workload": workload, "qos_interval_ms": 100,
                         "default_cpu_max": "80000 20000", "protected_cpu_max": "20000 20000"},
              "scope": "isolated Docker; runtime, rootfs, binaries, state and recordings on 1GiB tmpfs; host-side /tmp on 256MiB tmpfs; exported after shutdown; baseline includes idle cold daemon and observer; static quota only; host PSI is system-wide",
              "runs": []}
    write_json(output/"plan.json", {**report, "order": plan})
    try:
        core = launch(["/lab/bin/mockcore", "-iterations=50000", "-concurrency=4"], core_group, "mockcore", environment)
        client, core_client = Client("http://127.0.0.1:8080", token), Client("http://127.0.0.1:8081")
        wait_until(lambda: core_client.request("/healthz")[0] == 200, "core readiness")
        for repeat, group in plan:
            name = f"r{repeat}-{group}"
            baseline = group == "core-baseline"
            mode = "cold" if baseline else "warm"
            # cgroup v2 forbids internal processes after controllers have
            # been enabled. Give each daemon a fresh delegated subtree so
            # its pre-exec placement happens before New creates the leaves.
            engine_group = lab/name
            engine_group.mkdir()
            print(f"Starting {name}: {duration}s measurement, {edge_seconds}s edges", flush=True)
            daemon = launch(["/lab/bin/agentboxd", "-runsc="+str(runtime_wrapper), "-rootfs=/lab/rootfs",
                             "-state-dir=/lab/state/"+name, "-cgroup-root=auto", "-platform="+platform, "-mode="+mode, "-concurrency=2",
                             "-qos=observe", "-qos-interval=100ms", "-qos-protected-cgroup="+str(core_group)],
                            engine_group, name, dict(environment, AGENTBOX_API_TOKEN=token))
            parent = engine_group/"sandboxes"

            def ready():
                code, status = client.request("/v1/status")
                q = status.get("qos", {})
                return (code == 200 and status["ready"] and status["mode"] == mode
                        and (baseline or status["idle"] == 3) and q.get("state") == "observing" and not q["stale"]
                        and {"host", "sandboxes", "protected"}.issubset({s["name"] for s in q["sources"]}) and all(s["available"] and s["interval_ms"] is not None for s in q["sources"]))
            wait_until(ready, name+" readiness")
            if baseline and any(p.is_dir() for p in parent.iterdir()):
                raise RuntimeError("baseline has sandbox inventory")
            quota = report["config"]["protected_cpu_max" if group == "protected-quota" else "default_cpu_max"]
            # Only this isolated experiment controller writes the test quota.
            # Do not introduce request-controlled or production quota changes.
            (parent/"cpu.max").write_text(quota)
            before = {"sandboxes": limits(parent), "protected": limits(core_group), "outer": limits(root)}
            # Warm up the core and let observation baselines settle after setup.
            for _ in range(20):
                if core_client.request("/work")[0] != 200:
                    raise RuntimeError("core warmup failed")
            if not baseline:
                def warm_sandbox(worker):
                    code, result = client.request("/v1/execute", {"runtime": "python", "code": workload["code"],
                        "timeout_ms": workload["timeout_ms"], "request_id": f"{name}-warmup-{worker}"})
                    if (code != 200 or result.get("status") != "succeeded" or not result.get("cleanup_complete")
                            or result.get("stdout") != workload["expected_stdout"]):
                        write_json(output/f"{name}-warmup-{worker}-error.json", {"http_status": code, "result": result})
                        raise RuntimeError("sandbox warmup failed")
                with ThreadPoolExecutor(max_workers=2) as executor:
                    list(executor.map(warm_sandbox, range(2)))
                wait_until(ready, name+" inventory after warmup")
            time.sleep(1)
            result = run_phase(name, duration+2*edge_seconds, rate, 0 if baseline else 2, core_client, client, output,
                               workload=workload, load_start=edge_seconds, load_end=edge_seconds+duration)
            after = {"sandboxes": limits(parent), "protected": limits(core_group), "outer": limits(root)}
            result.update(group=group, repeat=repeat, limits_before=before, limits_after=after)
            write_json(output/(name+"-summary.json"), result)
            check_phase(result, baseline, rate, duration+2*edge_seconds)
            if before != after:
                raise RuntimeError("limits changed during measurement")
            if report["runs"] and result["workload_checksums"] != report["runs"][0]["workload_checksums"]:
                raise RuntimeError("core workload changed between groups")
            stop(daemon)
            if any(Path("/lab/state", name, "bundles").iterdir()) or any(p.is_dir() for p in parent.iterdir()):
                raise RuntimeError("sandbox resources survived shutdown")
            result["cleanup_complete"] = True
            report["runs"].append(result)
            p99 = result["load_window_core"]["success_scheduled_p99_ms"]
            p99_text = "unavailable" if p99 is None else f"{p99:.2f}ms"
            print(f"Completed {name}: core P99={p99_text}; "
                  f"sandbox={result['sandbox_completions_per_second']:.3f}/s", flush=True)
            time.sleep(1)
        stop(core)
        report["checks"] = ["rotating-order", "fixed-workloads", "three-observation-sources", "fixed-limits-per-run", "clean-shutdown"]
        write_json(output/"experiment.json", report)
        print("Static comparison complete", flush=True)
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
        try:
            while os.waitpid(-1, os.WNOHANG)[0]:
                pass
        except ChildProcessError:
            pass
        for path in output.iterdir():
            if path.is_file():
                destination = Path("/results")/path.name
                shutil.copyfile(path, destination)
                destination.chmod(0o644)


if __name__ == "__main__":
    main()
