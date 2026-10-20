#!/usr/bin/env python3
"""Summarize the bounded Gateway acceptance run, retaining every latency sample."""

import argparse
from datetime import date
import hashlib
import json
import math
from pathlib import Path
import statistics


def describe(values):
    ordered = sorted(values)
    return {"n": len(ordered), "median": statistics.median(ordered),
            "p90_nearest_rank": ordered[math.ceil(len(ordered)*0.9)-1],
            "min": ordered[0], "max": ordered[-1]}


def summarize(latency_log, recovery_log, systemd_log, allow_blocked_systemd=False, measurement_date=None):
    latency = latency_log.read_text()
    recovery = recovery_log.read_text()
    systemd = systemd_log.read_text()
    if "--- PASS: TestGatewayLatencyRuntime " not in latency:
        raise ValueError("latency test did not pass")
    if "--- PASS: TestGatewayValidationRuntime " not in recovery:
        raise ValueError("recovery/output test did not pass")
    rows = [json.loads(line.split("GATEWAY_SAMPLE ", 1)[1])
            for line in latency.splitlines() if "GATEWAY_SAMPLE " in line]
    identities = [(r["pair"], r["route"]) for r in rows]
    expected = {(i, route) for i in range(30) for route in ("unix-direct", "gateway")}
    if len(identities) != 60 or set(identities) != expected:
        raise ValueError("expected exactly 30 complete route pairs")
    if any(r["http"] != 200 or r["status"] != "succeeded" or not r["cleanup_complete"] for r in rows):
        raise ValueError("latency run contains failed or unclean executions")
    by_route = {route: [r for r in rows if r["route"] == route] for route in ("unix-direct", "gateway")}
    summaries = {route: {key: describe([r[key] for r in samples])
                         for key in ("wall_ms", "outside_engine_ms")}
                 for route, samples in by_route.items()}
    pairs = {i: {r["route"]: r for r in rows if r["pair"] == i} for i in range(30)}
    deltas = [pair["gateway"]["outside_engine_ms"]-pair["unix-direct"]["outside_engine_ms"]
              for pair in pairs.values()]
    if '{\n  "gateway_systemd"' in systemd:
        begin = systemd.index('{\n  "gateway_systemd"')
        deployment = json.loads(systemd[begin:].strip())
        if deployment["gateway_systemd"] != "passed":
            raise ValueError("paired systemd acceptance did not pass")
    elif allow_blocked_systemd and "Failed to create control group inotify object: Too many open files" in systemd:
        deployment = {"gateway_systemd": "blocked", "stage": "systemd PID 1 initialization, before installation",
            "error": "Failed to create control group inotify object: Too many open files",
            "container_exit_code": 255, "tests_executed": 0}
    else:
        raise ValueError("paired systemd acceptance did not pass; unexpected failure is not a blocked result")
    evidence = lambda path: {"name": path.name, "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
    return {"date": measurement_date, "units": "ms unless explicitly marked KiB/bytes",
        "environment": {"platform": "systrap", "mode": "warm", "warm_size": 2,
            "max_sandboxes": 4, "execution_concurrency": 1, "gateway_latency_max_inflight": 8,
            "gateway_recovery_max_inflight": 1, "container_cpus": 6, "container_memory": "5GiB",
            "dependencies": "container tmpfs; pinned Python image rootfs", "gateway_cgo_enabled": False,
            "network": "private container loopback plus Unix HTTP, no external network"},
        "method": {"warmup_pairs_excluded": 3, "measured_pairs": 30,
            "route_order": "alternating first route within each pair",
            "readiness": "wait for two idle sandboxes and no creation/destruction before each request",
            "task": "print(1)", "outside_engine_ms": "client wall_ms minus returned daemon timings.total_ms; includes client/server HTTP and serialization, not pure Gateway CPU time",
            "limits": "descriptive small sample, no P99/SLO/throughput claim; no normal-disk or KVM performance claim"},
        "summary": summaries, "paired_outside_engine_delta_ms": describe(deltas),
        "recovery_evidence": [line.split(": ", 1)[-1] for line in recovery.splitlines() if "GATEWAY_" in line],
        "systemd": deployment, "sources": [evidence(p) for p in (latency_log, recovery_log, systemd_log)],
        "samples": rows}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("latency", "recovery", "systemd", "output"):
        parser.add_argument("--"+name, type=Path, required=True)
    parser.add_argument("--allow-blocked-systemd", action="store_true",
                        help="retain the specific observed PID-1 inotify failure as blocked, never passed")
    parser.add_argument("--measurement-date", type=date.fromisoformat, required=True,
                        help="actual measurement date, independent of when logs are summarized")
    args = parser.parse_args()
    result = summarize(args.latency, args.recovery, args.systemd, args.allow_blocked_systemd, args.measurement_date.isoformat())
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2)+"\n")
    print(json.dumps(result["summary"], indent=2))


if __name__ == "__main__":
    main()
