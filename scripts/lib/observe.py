#!/usr/bin/env python3
"""Record core latency and AgentBox observations; never change resource limits."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import csv
from datetime import datetime, timezone
import http.client
import ipaddress
import json
import math
import os
from pathlib import Path
import threading
import time
from urllib.parse import urlsplit


class Client:
    def __init__(self, url, token=None):
        parsed = urlsplit(url)
        if (parsed.scheme != "http" or parsed.username or parsed.password or parsed.path not in ("", "/")
                or parsed.query or parsed.fragment or not ipaddress.ip_address(parsed.hostname).is_loopback
                or not parsed.port):
            raise ValueError("URLs must contain an explicit HTTP loopback IP and port")
        self.host, self.port, self.token = parsed.hostname, parsed.port, token

    def request(self, path, body=None):
        connection = http.client.HTTPConnection(self.host, self.port, timeout=45)
        headers = {}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        data = None if body is None else json.dumps(body).encode()
        if data is not None:
            headers["Content-Type"] = "application/json"
        try:
            connection.request("GET" if data is None else "POST", path, data, headers)
            response = connection.getresponse()
            payload = response.read((2 << 20) + 1)
            if len(payload) > 2 << 20:
                raise ValueError("response exceeds observation limit")
            return response.status, json.loads(payload)
        finally:
            connection.close()


def percentile(values, ratio):
    return sorted(values)[math.ceil(len(values)*ratio)-1] if values else None


def sandbox_workload(kind):
    if kind == "hog":
        return {"kind": kind, "code": "while True: pass", "timeout_ms": 2000,
                "expected_status": "timeout", "expected_stdout": None}
    if kind != "fixed":
        raise ValueError("sandbox workload must be hog or fixed")
    iterations = 8_000_000
    code = ("s = 1\n"
            f"for _ in range({iterations}):\n    s = (1664525*s + 1013904223) & 0xffffffff\nprint(f'{{s:08x}}')")
    return {"kind": kind, "iterations": iterations, "code": code, "timeout_ms": 20000,
            "expected_status": "succeeded", "expected_stdout": "cd496201\n"}


def core_metrics(rows):
    successful = [r for r in rows if r["http_status"] == 200 and not r["error"]]
    return {"core_offered": len(rows), "core_succeeded": len(successful),
            "core_rejected": sum(r["http_status"] == 503 for r in rows),
            "client_dropped": sum(r["client_drop"] for r in rows),
            "core_errors": sum(not r["client_drop"] and (r["http_status"] not in (200, 503) or bool(r["error"])) for r in rows),
            "success_wall_p50_ms": percentile([r["wall_ms"] for r in successful], .50),
            "success_wall_p99_ms": percentile([r["wall_ms"] for r in successful], .99),
            "success_scheduled_p50_ms": percentile([r["scheduled_wall_ms"] for r in successful], .50),
            "success_scheduled_p99_ms": percentile([r["scheduled_wall_ms"] for r in successful], .99),
            "send_lag_p99_ms": percentile([max(0, r["started_ms"]-r["scheduled_ms"]) for r in rows], .99)}


def summarize_sandboxes(rows, workload, load_start, load_end):
    successful = [r for r in rows if r["http_status"] == 200 and r["status"] == workload["expected_status"]
                  and r["cleanup_complete"] and not r["error"]]
    completed_in_window = sum(load_start*1000 <= r["finished_ms"] < load_end*1000 for r in successful)
    return {"sandbox_requests": len(rows), "sandbox_completed": len(successful),
            "sandbox_completed_timeouts": sum(r["status"] == "timeout" for r in successful),
            "sandbox_completed_in_load_window": completed_in_window,
            "sandbox_completions_per_second": completed_in_window/(load_end-load_start),
            "sandbox_wall_p50_ms": percentile([r["wall_ms"] for r in successful], .50),
            "sandbox_wall_p99_ms": percentile([r["wall_ms"] for r in successful], .99),
            "sandbox_rejected": sum(r["http_status"] == 503 for r in rows),
            "sandbox_errors": sum(r["http_status"] != 503 and r not in successful for r in rows)}


def run_phase(name, duration, rate, sandbox_workers, core, agent, directory, *, workload=None, load_start=0, load_end=None, rate_schedule=None):
    workload = workload or sandbox_workload("hog")
    load_end = duration if load_end is None else load_end
    if not 0 <= load_start < load_end <= duration:
        raise ValueError("load window must be inside the core request window")
    if rate_schedule is None:
        arrivals = [sequence/rate for sequence in range(int(duration*rate))]
    else:
        if not rate_schedule or rate_schedule[0][0] != 0:
            raise ValueError("rate schedule must start at zero")
        arrivals = []
        for i, (start, offered) in enumerate(rate_schedule):
            end = rate_schedule[i+1][0] if i+1 < len(rate_schedule) else duration
            if not 0 <= start < end <= duration or not 1 <= offered <= 100:
                raise ValueError("invalid rate schedule")
            arrivals.extend(start+n/offered for n in range(int((end-start)*offered)))
    origin = time.monotonic()
    stop_sampling, stop_sandboxes = threading.Event(), threading.Event()
    core_rows, sandbox_rows, observation_errors = [], [], []
    quality = {"frames": 0, "usable_frames": 0, "stale_frames": 0, "unavailable_frames": 0, "baseline_frames": 0}
    row_lock = threading.Lock()
    # Bound queued + running client work; report client drops separately.
    permits = threading.BoundedSemaphore(32)
    observations = (directory / f"{name}-observations.jsonl").open("x")

    def sample():
        while not stop_sampling.is_set():
            record = {"elapsed_ms": (time.monotonic()-origin)*1000}
            try:
                code, status = agent.request("/v1/status")
                record.update(http_status=code, status=status)
                if code != 200:
                    observation_errors.append("HTTP "+str(code))
                elif status.get("qos", {}).get("mode") not in ("observe", "enforce"):
                    observation_errors.append("observation report missing")
                else:
                    q = status["qos"]
                    quality["frames"] += 1
                    quality["usable_frames"] += int(not q["stale"] and {"host", "sandboxes", "protected"}.issubset({s["name"] for s in q["sources"]})
                                                    and all(s["available"] and s["interval_ms"] is not None for s in q["sources"]))
                    quality["stale_frames"] += int(q["stale"])
                    quality["unavailable_frames"] += int(any(not s["available"] for s in q["sources"]))
                    quality["baseline_frames"] += int(any(s["available"] and s["interval_ms"] is None for s in q["sources"]))
            except (OSError, ValueError, http.client.HTTPException) as error:
                record["error"] = str(error)
                observation_errors.append(str(error))
            observations.write(json.dumps(record)+"\n")
            observations.flush()
            stop_sampling.wait(0.1)

    def execute_sandboxes(worker):
        sequence = 0
        if stop_sandboxes.wait(max(0, origin+load_start-time.monotonic())):
            return
        while not stop_sandboxes.is_set() and time.monotonic() < origin+load_end:
            started = time.monotonic()
            row = {"worker": worker, "sequence": sequence, "started_ms": (started-origin)*1000}
            sequence += 1
            try:
                code, result = agent.request("/v1/execute", {"runtime": "python", "code": workload["code"],
                    "timeout_ms": workload["timeout_ms"], "request_id": f"{name}-{worker}-{sequence}"})
                row.update(http_status=code, status=result.get("status", ""), execution_id=result.get("execution_id", ""),
                           cleanup_complete=result.get("cleanup_complete", False), error="",
                           cpu_usec=result.get("usage", {}).get("cpu_usec", ""),
                           execution_ms=result.get("timings", {}).get("execution_ms", ""))
                row.update(rejection_reason=result.get("reason", ""), queue_ms=result.get("timings", {}).get("queue_ms", result.get("queue_ms", "")),
                           setup_ms=result.get("timings", {}).get("setup_ms", ""), cleanup_ms=result.get("timings", {}).get("cleanup_ms", ""),
                           total_ms=result.get("timings", {}).get("total_ms", ""), cleanup_phases_json=json.dumps(result["cleanup_phases"]) if "cleanup_phases" in result else "")
                if code == 200 and workload["expected_stdout"] is not None and result.get("stdout") != workload["expected_stdout"]:
                    row["error"] = "fixed sandbox workload checksum mismatch"
                if code not in (200, 503) or (code == 200 and (row["status"] != workload["expected_status"] or not row["cleanup_complete"])):
                    row["error"] = json.dumps(result)[:8192]
            except (OSError, ValueError, http.client.HTTPException) as error:
                row.update(http_status=0, status="", execution_id="", cleanup_complete=False, cpu_usec="", execution_ms="", error=str(error))
                row.update(rejection_reason="",queue_ms="",setup_ms="",cleanup_ms="",total_ms="",cleanup_phases_json="")
            finished = time.monotonic()
            row.update(wall_ms=(finished-started)*1000, finished_ms=(finished-origin)*1000)
            with row_lock:
                sandbox_rows.append(row)
            # Every iteration is a new workload request, never a retry of an
            # uncertain result. Back off rejected load to avoid a busy loop.
            stop_sandboxes.wait(0.1 if row["http_status"] != 200 else 0.01)

    def work(sequence, scheduled):
        started = time.monotonic()
        row = {"sequence": sequence, "scheduled_ms": (scheduled-origin)*1000,
               "started_ms": (started-origin)*1000, "client_drop": False, "error": ""}
        try:
            code, result = core.request("/work")
            row.update(http_status=code, work_ms=result.get("work_ms", ""), checksum=result.get("checksum", ""), iterations=result.get("iterations", ""))
            if code == 200 and (not isinstance(row["iterations"], int) or row["iterations"] < 1
                                or not isinstance(row["checksum"], str) or len(row["checksum"]) != 64):
                row["error"] = "invalid core response"
        except (OSError, ValueError, http.client.HTTPException) as error:
            row.update(http_status=0, work_ms="", checksum="", iterations="", error=str(error))
        finally:
            finished = time.monotonic()
            row.update(wall_ms=(finished-started)*1000, scheduled_wall_ms=(finished-scheduled)*1000)
            with row_lock:
                core_rows.append(row)
            permits.release()

    sampler = threading.Thread(target=sample)
    workers = [threading.Thread(target=execute_sandboxes, args=(i,)) for i in range(sandbox_workers)]
    sampler.start()
    for worker in workers:
        worker.start()
    try:
        with ThreadPoolExecutor(max_workers=32) as executor:
            for sequence, offset in enumerate(arrivals):
                scheduled = origin+offset
                delay = scheduled-time.monotonic()
                if delay > 0:
                    time.sleep(delay)
                if permits.acquire(blocking=False):
                    executor.submit(work, sequence, scheduled)
                else:
                    with row_lock:
                        core_rows.append({"sequence": sequence, "scheduled_ms": (scheduled-origin)*1000,
                            "started_ms": (time.monotonic()-origin)*1000, "client_drop": True,
                            "http_status": 0, "work_ms": "", "checksum": "", "iterations": "", "wall_ms": 0,
                            "scheduled_wall_ms": "", "error": "client capacity exhausted"})
    finally:
        stop_sandboxes.set()
        for worker in workers:
            worker.join()
        stop_sampling.set()
        sampler.join()
        observations.close()
    for filename, fields, rows in (
        (f"{name}-core.csv", ["sequence", "scheduled_ms", "started_ms", "http_status", "client_drop", "wall_ms", "scheduled_wall_ms", "work_ms", "checksum", "iterations", "error"], sorted(core_rows, key=lambda r: r["sequence"])),
        (f"{name}-sandboxes.csv", ["worker", "sequence", "started_ms", "finished_ms", "wall_ms", "http_status", "status", "execution_id", "cleanup_complete", "cpu_usec", "execution_ms", "rejection_reason", "queue_ms", "setup_ms", "cleanup_ms", "total_ms", "cleanup_phases_json", "error"], sandbox_rows),
    ):
        with (directory / filename).open("x", newline="") as stream:
            writer = csv.DictWriter(stream, fieldnames=fields)
            writer.writeheader()
            writer.writerows(rows)
    return {"phase": name, **core_metrics(core_rows),
            "load_window": {"start_seconds": load_start, "end_seconds": load_end},
            "load_window_core": core_metrics([r for r in core_rows if load_start*1000 <= r["scheduled_ms"] < load_end*1000]),
            "workload_checksums": sorted({r["checksum"] for r in core_rows if r["http_status"] == 200 and not r["error"]}),
            **summarize_sandboxes(sandbox_rows, workload, load_start, load_end),
            "observation_errors": len(observation_errors), "sample_quality": quality}


def record(core_url, agent_url, token, output, duration=10, rate=20, sandbox_workers=2):
    if not 1 <= duration <= 300 or not 1 <= rate <= 100 or not 1 <= sandbox_workers <= 4:
        raise ValueError("duration must be 1..300s, rate 1..100/s and sandbox workers 1..4")
    if len(token) < 32:
        raise ValueError("AGENTBOX_API_TOKEN must contain at least 32 characters")
    directory = Path(output)
    directory.mkdir(parents=True, exist_ok=True)
    core, agent = Client(core_url), Client(agent_url, token)
    code, initial = agent.request("/v1/status")
    if code != 200 or initial.get("qos", {}).get("mode") not in ("observe", "enforce"):
        raise ValueError("AgentBox must be running in observe or enforce mode")
    phases = [run_phase("idle-pool", duration, rate, 0, core, agent, directory),
              run_phase("sandbox-load", duration, rate, sandbox_workers, core, agent, directory)]
    report = {"recorded_at": datetime.now(timezone.utc).isoformat(), "duration_seconds_per_phase": duration,
              "core_offered_per_second": rate, "sandbox_workers": sandbox_workers, "initial_status": initial, "phases": phases,
              "qos_mode": initial["qos"]["mode"],
              "scope": "idle-pool includes warm inventory and observer; enforce may change CPU quota; functional recording without QoS benefit claims"}
    with (directory / "summary.json").open("x") as stream:
        json.dump(report, stream, indent=2)
        stream.write("\n")
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--core-url", default="http://127.0.0.1:8081")
    parser.add_argument("--agent-url", default="http://127.0.0.1:8080")
    parser.add_argument("--output", required=True)
    parser.add_argument("--duration", type=int, default=10)
    parser.add_argument("--rate", type=int, default=20)
    parser.add_argument("--sandbox-workers", type=int, default=2)
    args = parser.parse_args()
    report = record(args.core_url, args.agent_url, os.environ.get("AGENTBOX_API_TOKEN", ""), args.output,
                    args.duration, args.rate, args.sandbox_workers)
    print(json.dumps(report["phases"], indent=2))


if __name__ == "__main__":
    main()
