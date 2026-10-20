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
