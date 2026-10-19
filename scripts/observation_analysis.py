"""Read-only extraction of existing observation artifacts; no workload execution."""

import csv
import hashlib
import json
import math
from pathlib import Path
import statistics


def distribution(values):
    values = sorted(values)
    def quantile(q):
        return values[math.ceil(len(values)*q)-1] if values else None
    return {"count": len(values), "mean": statistics.mean(values) if values else None,
            "p50": quantile(.5), "p95": quantile(.95), "p99": quantile(.99), "max": max(values) if values else None}


def read_rows(path):
    with path.open() as stream:
        return list(csv.DictReader(stream))


def number(value):
    if value in (None, ""):
        return None
    value = float(value)
    return value if math.isfinite(value) else None


def matched_task_timings(rows, log_lines):
    # Request CSVs identify measured tasks. Warmup log events must not enter
    # the distribution simply because they are in the same daemon log.
    ids = {r["execution_id"] for r in rows if r.get("execution_id") and r["http_status"] == "200"
           and r["status"] == "succeeded" and r["cleanup_complete"] == "True" and not r.get("error")}
    matched = {}
    duplicates = set()
    for line in log_lines:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        execution_id = event.get("execution_id")
        if event.get("msg") != "execution completed" or execution_id not in ids:
            continue
        if execution_id in matched:
            duplicates.add(execution_id)
        matched[execution_id] = event
    if duplicates:
        raise ValueError("duplicate task completion events: "+", ".join(sorted(duplicates)))
    return list(matched.values()), len(ids-matched.keys())


def pool_series(records):
    keys = ("idle", "creating", "leased", "running", "destroying", "total", "queued", "queued_bytes", "creation_count", "creation_failures", "cleanup_failures")
    series = {key: [] for key in ("time", "ready", *keys)}
    for record in records:
        status = record.get("status", {}) if record.get("http_status") == 200 else {}
        series["time"].append(record["elapsed_ms"]/1000)
        series["ready"].append(status.get("ready"))
        for key in keys:
            series[key].append(number(status.get(key)))
    return series


def observed_counter_rate(records, key, begin, end):
    points = [(r["elapsed_ms"]/1000, number(r.get("status", {}).get(key))) for r in records
              if r.get("http_status") == 200 and begin <= r["elapsed_ms"]/1000 < end]
    points = [(t, v) for t, v in points if v is not None]
    if len(points) < 2:
        return {"available": False, "reason": "insufficient_samples"}
    if any(b[1] < a[1] or b[0] <= a[0] for a, b in zip(points, points[1:])):
        return {"available": False, "reason": "counter_or_time_reset"}
    elapsed, delta = points[-1][0]-points[0][0], points[-1][1]-points[0][1]
    return {"available": True, "observed_start_seconds": points[0][0], "observed_end_seconds": points[-1][0],
            "delta": delta, "per_second": delta/elapsed}


def usable_resource_frame(record):
    qos = record.get("status", {}).get("qos", {})
    sources = qos.get("sources", [])
    return (record.get("http_status") == 200 and qos.get("stale") is False
            and {"host", "sandboxes", "protected"}.issubset(s["name"] for s in sources)
            and all(s.get("available") is True and s.get("interval_ms") is not None for s in sources))


def load_run(directory, run):
    name = run["phase"]
    core = read_rows(directory/(name+"-core.csv"))
    tasks = read_rows(directory/(name+"-sandboxes.csv"))
    with (directory/(name+"-observations.jsonl")).open() as stream:
        observations = [json.loads(line) for line in stream]
    log = directory/(name+".log")
    timings, unmatched = matched_task_timings(tasks, log.read_text().splitlines() if log.exists() else [])
    begin, end = (run["load_window"][key] for key in ("start_seconds", "end_seconds"))
    window = [r for r in core if begin*1000 <= float(r["scheduled_ms"]) < end*1000]
    successful = [r for r in window if r["http_status"] == "200" and not r["error"]]
    duration_columns = ("scheduled_wall_ms", "wall_ms", "work_ms")
    durations = {key: distribution([v for r in successful if (v := number(r.get(key))) is not None]) for key in duration_columns}
    durations["send_lag_ms"] = distribution([max(0, float(r["started_ms"])-float(r["scheduled_ms"])) for r in window])
    stages = {key: distribution([v for event in timings if (v := number(event.get(key))) is not None])
              for key in ("queue_ms", "setup_ms", "execution_ms", "cleanup_ms", "total_ms")}
    bad_readiness = sum(r.get("http_status") != 200 or r.get("status", {}).get("ready") is not True for r in observations)
    bad_resources = sum(not usable_resource_frame(r) for r in observations)
    sequences = [r["status"]["qos"]["sequence"] for r in observations if r.get("http_status") == 200 and "sequence" in r.get("status", {}).get("qos", {})]
    sample_keys = {s.get("sampled_at") for r in observations for s in r.get("status", {}).get("qos", {}).get("sources", [])
                   if s["name"] == "sandboxes" and s.get("available") and s.get("sampled_at")}
    scopes = sorted({s["name"] for r in observations for s in r.get("status", {}).get("qos", {}).get("sources", [])})
    outcome = {"offered": len(window), "succeeded": len(successful),
               "rejected": sum(r["http_status"] == "503" for r in window),
               "client_dropped": sum(r["client_drop"] == "True" for r in window),
               "other_errors": sum(r["client_drop"] != "True" and (r["http_status"] not in ("200", "503") or bool(r["error"])) for r in window)}
    eligible = bool(observations) and bad_readiness == 0 and bad_resources == 0 and not run["core_errors"] and not run["sandbox_errors"] and not run["observation_errors"]
    summary = {"phase": name, "group": run["group"], "repeat": run["repeat"], "comparison_eligible": eligible,
               "core_window_outcomes": outcome, "core_ms": durations, "task_api_ms": stages,
               "matched_task_events": len(timings), "unmatched_task_ids": unmatched,
               "successful_creation_rate": observed_counter_rate(observations, "creation_count", begin, end),
               "observation_frames": len(observations), "unready_or_failed_status_frames": bad_readiness, "unusable_resource_frames": bad_resources,
               "unique_sandbox_resource_samples": len(sample_keys), "collector_sequences_seen": len(set(sequences)) if sequences else None,
               "scopes_observed": scopes, "scheduling_distributions_recorded": any("scheduling" in r.get("status", {}) for r in observations)}
    return {"run": run, "summary": summary, "core": core, "window_successes": successful,
            "task_events": timings, "observations": observations, "pool": pool_series(observations)}


def render(directory, destination, report, runs):
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from static_experiment import GROUPS
    colors = dict(zip(GROUPS, ("#64748b", "#dc6b37", "#087f8c")))
    def save(fig, name):
        for suffix in ("png", "svg"):
            fig.savefig(destination/(name+"."+suffix), dpi=150, bbox_inches="tight")
        plt.close(fig)
    def cdf(axis, values, label, color):
        values = sorted(v for v in values if v is not None)
        if values:
            axis.plot(values, [(i+1)/len(values) for i in range(len(values))], label=label, color=color)
    eligible = all(r["summary"]["comparison_eligible"] for r in runs)
    if eligible:
        fig, axes = plt.subplots(1, 3, figsize=(15, 4.5), constrained_layout=True)
        for axis, key, title in zip(axes, ("scheduled_wall_ms", "wall_ms", "work_ms"),
                                    ("Planned send to response", "Client request call to response", "Server work elapsed")):
            for group in GROUPS:
                cdf(axis, [number(row.get(key)) for r in runs if r["run"]["group"] == group for row in r["window_successes"]], group, colors[group])
            axis.set(xlabel="ms", ylabel="ECDF", title=title)
            axis.legend(fontsize=8); axis.grid(alpha=.2)
        fig.suptitle("Same successful Core request cohort | pooled distributions, separate timing boundaries")
        save(fig, "core-latency-layers")
        fig, axes = plt.subplots(1, 4, figsize=(16, 4.5), constrained_layout=True)
        for axis, key in zip(axes, ("queue_ms", "setup_ms", "execution_ms", "cleanup_ms")):
            for group in GROUPS:
                cdf(axis, [number(e.get(key)) for r in runs if r["run"]["group"] == group for e in r["task_events"]], group, colors[group])
            axis.set(xlabel="ms", ylabel="ECDF", title=key)
            axis.grid(alpha=.2)
            if axis.lines: axis.legend(fontsize=8)
        fig.suptitle("Measured successful sandbox IDs only | warmup excluded | per-request API stages")
        save(fig, "sandbox-stages")
    for entry in runs:
        run, pool, rows = entry["run"], entry["pool"], entry["window_successes"]
        fig, axes = plt.subplots(4, 1, figsize=(12, 10), sharex=True, constrained_layout=True)
        for key, label, color in (("scheduled_wall_ms", "Planned", "#dc6b37"), ("wall_ms", "Request call", "#64748b"), ("work_ms", "Server work", "#087f8c")):
            axes[0].scatter([float(r["scheduled_ms"])/1000 for r in rows], [number(r.get(key)) for r in rows], s=6, alpha=.4, label=label, color=color)
        axes[0].set_ylabel("Core ms")
        axes[1].scatter([float(r["scheduled_ms"])/1000 for r in rows], [max(0,float(r["started_ms"])-float(r["scheduled_ms"])) for r in rows],s=6,color="#7c3aed")
        axes[1].set_ylabel("Client dispatch lag ms")
        for key in ("idle", "creating", "running", "destroying"):
            axes[2].step(pool["time"],pool[key],where="post",label=key)
        axes[2].set_ylabel("Instances")
        axes[3].step(pool["time"],pool["queued"],where="post",label="Queued requests",color="#087f8c")
        axes[3].set(xlabel="Seconds since phase start",ylabel="Queue size")
        for axis in axes:
            axis.axvspan(run["load_window"]["start_seconds"],run["load_window"]["end_seconds"],alpha=.08,color="#64748b")
            axis.grid(alpha=.2); axis.set_ylim(bottom=0)
            if axis is not axes[1]: axis.legend(fontsize=8,loc="upper right")
        fig.suptitle(run["phase"]+" | "+("comparison eligible" if entry["summary"]["comparison_eligible"] else "DIAGNOSTIC ONLY: invalid comparison"))
        save(fig,run["phase"]+"-supply")
    return matplotlib.__version__


def analyze(directory, destination, plots=True):
    directory, destination = Path(directory), Path(destination)
    destination.mkdir(parents=True, exist_ok=False)
    report = json.loads((directory/"experiment.json").read_text())
    runs = [load_run(directory, run) for run in report["runs"]]
    sources = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in directory.iterdir()
               if p.is_file() and (p.suffix in (".csv", ".jsonl", ".log") or p.name == "experiment.json")}
    result = {"source_directory": str(directory.resolve()), "new_load_executed": False, "source_sha256": sources,
              "scope": "Core metrics use the same successful window cohort; task stages match measured CSV execution IDs, including drain, excluding warmup; no percentile subtraction",
              "comparison_eligible": all(r["summary"]["comparison_eligible"] for r in runs), "runs": [r["summary"] for r in runs]}
    with (destination/"analysis.json").open("x") as stream:
        json.dump(result,stream,indent=2); stream.write("\n")
    fields = ["phase","group","repeat","comparison_eligible","planned_p99_ms","request_p99_ms","server_work_p99_ms","dispatch_lag_p99_ms","creation_rate_per_second","matched_task_events","unmatched_task_ids","unique_sandbox_resource_samples"]
    with (destination/"analysis.csv").open("x",newline="") as stream:
        writer=csv.DictWriter(stream,fieldnames=fields); writer.writeheader()
        for r in runs:
            s=r["summary"]; row={k:s.get(k) for k in fields}
            for name,column in (("planned","scheduled_wall_ms"),("request","wall_ms"),("server_work","work_ms"),("dispatch_lag","send_lag_ms")):
                row[name+"_p99_ms"]=s["core_ms"][column]["p99"]
            row["creation_rate_per_second"]=s["successful_creation_rate"].get("per_second")
            writer.writerow(row)
    if plots:
        version=render(directory,destination,report,runs)
        (destination/"plot-metadata.json").write_text(json.dumps({"matplotlib":version,"analysis_sha256":hashlib.sha256((destination/"analysis.json").read_bytes()).hexdigest()},indent=2)+"\n")
    return result
