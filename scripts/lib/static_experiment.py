"""Shared plan and data-quality checks for the static CPU comparison."""

GROUPS = ("core-baseline", "default-quota", "protected-quota")


def experiment_plan(repetitions):
    if not 1 <= repetitions <= 9:
        raise ValueError("repetitions must be 1..9")
    return [(repeat+1, group) for repeat in range(repetitions)
            for group in GROUPS[repeat % len(GROUPS):]+GROUPS[:repeat % len(GROUPS)]]


def check_phase(report, baseline, rate, duration):
    if report["core_offered"] != rate*duration or report["core_succeeded"] == 0:
        raise RuntimeError("core request count or workload is invalid")
    if any(report[key] for key in ("core_errors", "observation_errors", "sandbox_errors")):
        raise RuntimeError("experiment contains request errors; inspect raw records")
    if len(report["workload_checksums"]) != 1:
        raise RuntimeError("core workload changed")
    if (baseline and report["sandbox_requests"]) or (not baseline and not report["sandbox_completed"]):
        raise RuntimeError("sandbox workload did not match the experiment group")
    # Overload is a measured outcome, never silently discarded. Require enough
    # valid observations to interpret it, while preserving all invalid frames.
    quality = report["sample_quality"]
    if quality["usable_frames"] == 0 or quality["unavailable_frames"]:
        raise RuntimeError("missing observation sources during measurement")


def write_json(path, value):
    import json
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def dynamic_summary(directory, name, phase_seconds, slo_ms):
    """Fixed-window results include rejects/drops; recovery needs 3 good seconds."""
    import csv
    import json
    from observe import percentile
    with (directory/(name+"-core.csv")).open() as stream:
        core = list(csv.DictReader(stream))
    with (directory/(name+"-sandboxes.csv")).open() as stream:
        tasks = list(csv.DictReader(stream))
    frames = [json.loads(line) for line in (directory/(name+"-observations.jsonl")).read_text().splitlines()]
    if any(f.get("http_status") != 200 or not f.get("status", {}).get("ready") for f in frames):
        raise RuntimeError("unhealthy executor invalidates CPU benefit comparison")
    def good(row):
        return row["http_status"] == "200" and not row["error"] and row["client_drop"] == "False"
    def violation(rows):
        return any(not good(r) for r in rows) or (percentile([float(r["scheduled_wall_ms"]) for r in rows if good(r)], .99) or 0) > slo_ms
    windows = []
    for i, label in enumerate(("low-before", "high", "low-after")):
        start, end = i*phase_seconds, (i+1)*phase_seconds
        rows = [r for r in core if start*1000 <= float(r["scheduled_ms"]) < end*1000]
        seconds = [[r for r in rows if second*1000 <= float(r["scheduled_ms"]) < (second+1)*1000] for second in range(start, end)]
        quota = []
        for frame in frames:
            if not start*1000 <= frame["elapsed_ms"] < end*1000:
                continue
            status = frame.get("status", {})
            if not status.get("ready") or status.get("qos", {}).get("stale"):
                continue
            control = status["qos"].get("cpu_control", {})
            if control.get("write_failures", 0) or control.get("state") == "degraded":
                raise RuntimeError("CPU control degraded; cannot claim benefit")
            for source in status["qos"]["sources"]:
                if source["name"] == "sandboxes" and source.get("available"):
                    c = source["sample"]["cgroup"]
                    quota.append(c["cpu_quota_usec"]["value"]/c["cpu_period_usec"])
        completed = [r for r in tasks if start*1000 <= float(r["finished_ms"]) < end*1000
                     and r["status"] == "succeeded" and r["cleanup_complete"] == "True" and not r["error"]]
        bad = [violation(bucket) if bucket else None for bucket in seconds]
        recovery = next((j for j in range(len(bad)-2) if bad[j:j+3] == [False]*3), None)
        windows.append({"phase": label, "offered": len(rows), "failed_or_dropped": sum(not good(r) for r in rows),
                        "core_p99_ms": percentile([float(r["scheduled_wall_ms"]) for r in rows if good(r)], .99),
                        "slo_violating_seconds": sum(v is True for v in bad), "missing_seconds": sum(v is None for v in bad),
                        "first_three_good_seconds_start": recovery, "sandbox_completed": len(completed),
                        "cpu_min": min(quota) if quota else None, "cpu_max": max(quota) if quota else None})
    return {"slo_ms": slo_ms, "phases": windows,
            "interpretation": "one-second P99 uses few samples in low phases; exploratory comparison; no statistical superiority claim"}
