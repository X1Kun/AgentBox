#!/usr/bin/env python3
"""Render static CPU comparison artifacts; no service access or resource writes."""

import argparse
import csv
import hashlib
import json
import math
from pathlib import Path
import statistics

from static_experiment import GROUPS

LABELS = {"core-baseline": "Core baseline (cold idle daemon)", "default-quota": "Warm load / default 4 CPU cap",
          "protected-quota": "Warm load / static 1 CPU cap"}
COLORS = {"core-baseline": "#64748b", "default-quota": "#dc6b37", "protected-quota": "#087f8c"}


def core_rows(directory, run):
    with (directory/(run["phase"]+"-core.csv")).open() as stream:
        return list(csv.DictReader(stream))


def window_latencies(directory, run):
    begin, end = (run["load_window"][key]*1000 for key in ("start_seconds", "end_seconds"))
    return [float(r["scheduled_wall_ms"]) for r in core_rows(directory, run)
            if r["http_status"] == "200" and not r["error"] and begin <= float(r["scheduled_ms"]) < end]


def observation_series(records, name):
    series = {key: [] for key in ("time", "psi", "cpu", "quota", "throttled")}
    for record in records:
        series["time"].append(record["elapsed_ms"]/1000)
        report = record.get("status", {}).get("qos", {})
        source = next((s for s in report.get("sources", []) if s["name"] == name), None)
        sample = source.get("sample") if source else None
        valid = (not report.get("stale", True) and source and source["available"]
                 and source["interval_ms"] is not None and sample)
        group = sample.get("cgroup") if sample else None
        delta = group.get("delta") if group and valid else None
        series["psi"].append(sample["cpu_pressure"]["some"]["stall_percent"] if valid else math.nan)
        series["cpu"].append(delta["cpu_usage_cores"] if delta else math.nan)
        quota = group["cpu_quota_usec"] if group else None
        series["quota"].append(quota["value"]/group["cpu_period_usec"] if valid and quota and not quota["unlimited"] else math.nan)
        series["throttled"].append(100*delta["cpu_throttled_periods"]/delta["cpu_periods"] if delta and delta["cpu_periods"] else math.nan)
    return series


def validate_readiness(records):
    if not records:
        raise ValueError("no observation records")
    for record in records:
        if record.get("http_status") != 200 or record.get("status", {}).get("ready") is not True:
            raise ValueError("executor became unavailable; these records are not a valid static-quota comparison")


def save_figure(figure, directory, name, plt):
    for suffix in ("png", "svg"):
        figure.savefig(directory/(name+"."+suffix), dpi=160, bbox_inches="tight")
    plt.close(figure)


def timeline(directory, destination, run, records, plt):
    sandboxes, protected = observation_series(records, "sandboxes"), observation_series(records, "protected")
    rows = core_rows(directory, run)
    successes = [r for r in rows if r["http_status"] == "200" and not r["error"]]
    figure, axes = plt.subplots(4, 1, figsize=(12, 10), sharex=True, constrained_layout=True)
    axes[0].scatter([float(r["scheduled_ms"])/1000 for r in successes],
                    [float(r["scheduled_wall_ms"]) for r in successes], s=8, alpha=.5, color="#334155", label="Successful requests")
    for label, selected, color in (
        ("Rejected", [r for r in rows if r["http_status"] == "503"], "#dc2626"),
        ("Client drops / errors", [r for r in rows if r["http_status"] not in ("200", "503") or r["error"]], "#9333ea"),
    ):
        if selected:
            axes[0].scatter([float(r["scheduled_ms"])/1000 for r in selected], [0]*len(selected), marker="x", color=color, label=label+" (marked at zero)")
    axes[0].set_ylabel("Core response (ms)\nfrom scheduled send")
    for name, series, color in (("Protected", protected, "#087f8c"), ("Sandboxes", sandboxes, "#dc6b37")):
        axes[1].plot(series["time"], series["psi"], color=color, linewidth=1, label=name+" CPU some")
    axes[1].set_ylabel("CPU PSI (%)")
    axes[2].plot(sandboxes["time"], sandboxes["cpu"], color="#dc6b37", linewidth=1, label="Sandbox CPU usage")
    axes[2].step(sandboxes["time"], sandboxes["quota"], where="post", color="#64748b", linestyle="--", label="Sandbox CPU cap")
    outer_quota, outer_period = run["limits_before"]["outer"]["cpu.max"].split()
    if outer_quota != "max":
        axes[2].axhline(int(outer_quota)/int(outer_period), color="#94a3b8", linestyle=":", label="Shared outer CPU cap")
    axes[2].set_ylabel("CPU equivalents")
    axes[3].plot(sandboxes["time"], sandboxes["throttled"], color="#7c3aed", linewidth=1, label="Throttled / active periods")
    axes[3].set_ylabel("Throttled periods (%)")
    axes[3].set_xlabel("Seconds since phase start")
    begin, end = (run["load_window"][key] for key in ("start_seconds", "end_seconds"))
    for axis in axes:
        axis.axvspan(begin, end, color="#94a3b8", alpha=.1)
        axis.axvline(begin, color="#94a3b8", linestyle=":")
        axis.axvline(end, color="#94a3b8", linestyle=":")
        axis.legend(loc="upper right", fontsize=8)
        axis.grid(alpha=.2)
        axis.set_ylim(bottom=0)
    figure.suptitle(f"{run['phase']} | {LABELS[run['group']]}\n"
                   "Shaded: comparison window; invalid observation windows are gaps; in-flight tasks may finish after load ends", fontsize=11)
    save_figure(figure, destination, run["phase"]+"-timeline", plt)


def comparison(directory, destination, report, plt):
    figure, axes = plt.subplots(1, 3, figsize=(15, 5), constrained_layout=True)
    for position, group in enumerate(GROUPS):
        runs = [r for r in report["runs"] if r["group"] == group]
        values = sorted(v for run in runs for v in window_latencies(directory, run))
        if values:
            axes[0].plot(values, [(i+1)/len(values) for i in range(len(values))], color=COLORS[group], label=group)
        for axis, key in ((axes[1], "success_scheduled_p99_ms"), (axes[2], "sandbox_completions_per_second")):
            points = [r["load_window_core"][key] if axis is axes[1] else r[key] for r in runs]
            points = [p for p in points if p is not None]
            if points:
                offsets = [(i-(len(points)-1)/2)*.09 for i in range(len(points))]
                axis.scatter([position+o for o in offsets], points, color=COLORS[group], zorder=3)
                axis.plot([position-.2, position+.2], [statistics.median(points)]*2, color=COLORS[group], linewidth=2)
    axes[0].set(xlabel="Core response from scheduled send (ms)", ylabel="ECDF of successful requests", title="Successful latency distribution (pooled)")
    axes[0].legend(fontsize=8)
    axes[1].set(ylabel="Core P99 (ms)", title="Per-run P99; line = median of runs")
    axes[2].set(ylabel="Fixed sandbox completions / second", title="Completions inside load window")
    for axis in axes[1:]:
        axis.set_xticks(range(len(GROUPS)), ["Baseline", "Default\n4 CPU cap", "Protected\n1 CPU cap"])
    for axis in axes:
        axis.grid(alpha=.2)
        axis.set_ylim(bottom=0)
    figure.suptitle(f"Static CPU comparison | {report['platform']} | isolated Docker, 2 CPU outer cap\n"
                   "Successful responses only in latency plots; rejection/drop/error counts are retained in comparison.csv", fontsize=11)
    save_figure(figure, destination, "comparison", plt)
    fields = ["phase", "group", "repeat", "core_offered", "core_succeeded", "core_rejected", "client_dropped", "core_errors",
              "success_scheduled_p50_ms", "success_scheduled_p99_ms", "send_lag_p99_ms", "sandbox_completed_in_load_window",
              "sandbox_completions_per_second", "sandbox_wall_p50_ms", "sandbox_wall_p99_ms", "sandbox_rejected", "sandbox_errors",
              "observation_errors", "stale_frames", "unavailable_frames", "baseline_frames"]
    with (destination/"comparison.csv").open("w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        for run in report["runs"]:
            row = {key: run.get(key, "") for key in fields}
            row.update({k: run["load_window_core"][k] for k in fields if k in run["load_window_core"]})
            row.update({k: run["sample_quality"][k] for k in ("stale_frames", "unavailable_frames", "baseline_frames")})
            writer.writerow(row)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path, help="directory containing experiment.json and raw records")
    args = parser.parse_args()
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError as error:
        raise SystemExit("Install plotting dependencies from scripts/plot-requirements.txt") from error
    report = json.loads((args.directory/"experiment.json").read_text())
    observations = {}
    for run in report["runs"]:
        with (args.directory/(run["phase"]+"-observations.jsonl")).open() as stream:
            records = [json.loads(line) for line in stream]
        validate_readiness(records)
        observations[run["phase"]] = records
    destination = args.directory/"figures"
    destination.mkdir(exist_ok=True)
    plt.rcParams.update({"font.size": 10, "axes.spines.top": False, "axes.spines.right": False})
    comparison(args.directory, destination, report, plt)
    for run in report["runs"]:
        timeline(args.directory, destination, run, observations[run["phase"]], plt)
    (destination/"plot-metadata.json").write_text(json.dumps({"matplotlib": matplotlib.__version__, "source": "../experiment.json",
        "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "executor_readiness_checked": True,
        "latency_unit": "ms from scheduled send", "invalid_samples": "gaps", "p99": "per-run nearest-rank; not averaged or a short-window time series"}, indent=2)+"\n")
    print("Figures: "+str(destination))


if __name__ == "__main__":
    main()
