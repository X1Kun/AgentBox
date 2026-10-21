"""Regression tests for measurement boundaries and invalid-data handling."""

import importlib.util
from contextlib import redirect_stdout
import io
import json
import math
from pathlib import Path
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]/"scripts/lib"))
from observe import core_metrics, run_phase, sandbox_workload, summarize_sandboxes
from static_experiment import GROUPS, check_phase, experiment_plan, dynamic_summary
from observation_analysis import distribution, matched_task_timings, observed_counter_rate, pool_series, usable_resource_frame

spec = importlib.util.spec_from_file_location("plot_static", Path(__file__).resolve().parents[2]/"scripts/lib/plot_static.py")
plot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plot)



class MeasurementTests(unittest.TestCase):
    def test_dynamic_summary_counts_failures_and_drain_boundaries(self):
        import csv
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            core = [dict(http_status="200", error="", client_drop="False", scheduled_ms=n*1000, scheduled_wall_ms=10) for n in range(9)]
            core[4].update(http_status="503", scheduled_wall_ms=1)
            tasks = [dict(finished_ms=end, status="succeeded", cleanup_complete="True", error="") for end in (1000, 4000, 8999, 9000)]
            for suffix, rows in (("core", core), ("sandboxes", tasks)):
                with (output/("trial-"+suffix+".csv")).open("w") as stream:
                    writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
                    writer.writeheader(); writer.writerows(rows)
            frames = [dict(elapsed_ms=n*1000, http_status=200, status=dict(ready=True, qos=dict(stale=False, sources=[]))) for n in range(9)]
            (output/"trial-observations.jsonl").write_text("".join(json.dumps(f)+"\n" for f in frames))
            result = dynamic_summary(output, "trial", 3, 50)
            self.assertEqual([p["sandbox_completed"] for p in result["phases"]], [1, 1, 1])
            self.assertEqual(result["phases"][1]["slo_violating_seconds"], 1)
            self.assertEqual(result["phases"][2]["first_three_good_seconds_start"], 0)
            self.assertIsNone(result["phases"][1]["first_three_good_seconds_start"])

    def test_recorder_modes_preserve_raw_frames_and_quality(self):
        for mode in ("observe", "enforce", "off", None):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                sampled = threading.Event()
                status = {"ready": True, "qos": {
                    "mode": mode, "stale": False,
                    "sources": [dict(name=name, available=True, interval_ms=100)
                                for name in ("host", "sandboxes", "protected")],
                }}

                class Agent:
                    def request(self, path):
                        if path != "/v1/status":
                            raise AssertionError("unexpected workload request")
                        sampled.set()
                        return 200, status

                class Core:
                    def request(self, path):
                        if path != "/work" or not sampled.wait(2):
                            raise AssertionError("sampler did not run")
                        return 200, {"iterations": 1, "checksum": "a"*64, "work_ms": .1}

                output = Path(directory)
                report = run_phase("mode-check", 1, 1, 0, Core(), Agent(), output)
                frames = [json.loads(line) for line in
                          (output/"mode-check-observations.jsonl").read_text().splitlines()]
                self.assertGreaterEqual(len(frames), 1)
                self.assertTrue(all(frame["http_status"] == 200 and frame["status"] == status
                                    for frame in frames))
                self.assertEqual(report["core_succeeded"], 1)
                valid = mode in ("observe", "enforce")
                self.assertEqual(report["observation_errors"], 0 if valid else len(frames))
                self.assertEqual(report["sample_quality"], {
                    "frames": len(frames) if valid else 0,
                    "usable_frames": len(frames) if valid else 0,
                    "stale_frames": 0, "unavailable_frames": 0, "baseline_frames": 0,
                })

    def test_analysis_rechecks_raw_resource_validity(self):
        sources = [dict(name=name, available=True, interval_ms=100) for name in ("host", "sandboxes", "protected", "manager")]
        frame = {"http_status": 200, "status": {"qos": {"stale": False, "sources": sources}}}
        self.assertTrue(usable_resource_frame(frame))
        sources[-1]["available"] = False
        self.assertFalse(usable_resource_frame(frame))
        sources.pop()
        frame["status"]["qos"]["stale"] = True
        self.assertFalse(usable_resource_frame(frame))
        self.assertFalse(usable_resource_frame({"http_status": 200, "status": {"ready": True}}))

    def test_stage_extraction_excludes_warmup_and_detects_duplicate_logs(self):
        rows=[dict(execution_id="measured",http_status="200",status="succeeded",cleanup_complete="True",error="")]
        events=['{"msg":"execution completed","execution_id":"warmup","setup_ms":999}',
                '{"msg":"execution completed","execution_id":"measured","setup_ms":12}']
        matched,missing=matched_task_timings(rows,events)
        self.assertEqual((len(matched),missing,matched[0]["setup_ms"]),(1,0,12))
        with self.assertRaises(ValueError):matched_task_timings(rows,events+[events[-1]])

    def test_supply_counter_resets_and_missing_gauges_stay_unknown(self):
        records=[{"elapsed_ms":i*1000,"http_status":200,"status":{"ready":True,"creation_count":v,"idle":0}} for i,v in enumerate((2,3,5))]
        self.assertEqual(observed_counter_rate(records,"creation_count",0,3)["per_second"],1.5)
        records[-1]["status"]["creation_count"]=0
        self.assertFalse(observed_counter_rate(records,"creation_count",0,3)["available"])
        series=pool_series(records)
        self.assertEqual(series["idle"],[0,0,0]);self.assertEqual(series["creating"],[None,None,None])
        self.assertIsNone(distribution([])["p99"])

    def test_rotating_order_preserves_all_groups(self):
        plan = experiment_plan(3)
        for group in GROUPS:
            self.assertEqual({i % 3 for i, (_, g) in enumerate(plan) if g == group}, {0, 1, 2})
        self.assertEqual(len(plan), 9)
        for value in (0, 10):
            with self.assertRaises(ValueError):
                experiment_plan(value)

    def test_client_wait_and_failed_requests_are_not_hidden(self):
        rows = [dict(http_status=200, error="", client_drop=False, wall_ms=10, scheduled_wall_ms=60, started_ms=50, scheduled_ms=0),
                dict(http_status=503, error="", client_drop=False, wall_ms=1, scheduled_wall_ms=1, started_ms=100, scheduled_ms=100),
                dict(http_status=0, error="capacity", client_drop=True, wall_ms=0, scheduled_wall_ms="", started_ms=200, scheduled_ms=200),
                dict(http_status=0, error="network", client_drop=False, wall_ms=100, scheduled_wall_ms=100, started_ms=300, scheduled_ms=300)]
        report = core_metrics(rows)
        self.assertEqual((report["core_offered"], report["core_succeeded"], report["core_rejected"], report["client_dropped"], report["core_errors"]), (4, 1, 1, 1, 1))
        self.assertEqual(report["success_wall_p99_ms"], 10)
        self.assertEqual(report["success_scheduled_p99_ms"], 60)
        self.assertEqual(report["send_lag_p99_ms"], 50)

    def test_drain_tail_does_not_inflate_throughput(self):
        rows = [dict(http_status=200, status="succeeded", cleanup_complete=True, error="", finished_ms=end, wall_ms=100)
                for end in (5100, 14999, 15000, 16000)]
        rows.append(dict(http_status=200, status="succeeded", cleanup_complete=True, error="bad checksum", finished_ms=10000, wall_ms=100))
        report = summarize_sandboxes(rows, {"expected_status": "succeeded"}, 5, 15)
        self.assertEqual(report["sandbox_completed"], 4)
        self.assertEqual(report["sandbox_completed_in_load_window"], 2)
        self.assertEqual(report["sandbox_completions_per_second"], .2)
        self.assertEqual(report["sandbox_errors"], 1)

    def test_invalid_observations_are_gaps_not_zero(self):
        def record(stale=False, available=True, interval=100, delta=None):
            return {"elapsed_ms": 100, "status": {"qos": {"stale": stale, "sources": [{"name": "sandboxes", "available": available,
                    "interval_ms": interval, "sample": {"cpu_pressure": {"some": {"stall_percent": 0}},
                    "cgroup": {"delta": delta, "cpu_quota_usec": {"unlimited": False, "value": 20000}, "cpu_period_usec": 20000}}}]}}}
        delta = {"cpu_usage_cores": 0, "cpu_throttled_periods": 0, "cpu_periods": 0}
        records = [record(stale=True, delta=delta), record(available=False, delta=delta), record(interval=None, delta=delta), record(delta=delta), {"elapsed_ms": 200, "error": "unavailable"}]
        series = plot.observation_series(records, "sandboxes")
        self.assertTrue(all(math.isnan(series["psi"][i]) for i in (0, 1, 2, 4)))
        self.assertEqual(series["psi"][3], 0)
        self.assertEqual(series["cpu"][3], 0)
        self.assertTrue(math.isnan(series["throttled"][3]))

    def test_fixed_work_has_known_checksum_and_finite_code(self):
        workload = sandbox_workload("fixed")
        self.assertEqual(workload["iterations"], 8_000_000)
        self.assertEqual(workload["expected_status"], "succeeded")
        # Independent fixed vector for the exact workload used by real runs.
        self.assertEqual(workload["expected_stdout"], "cd496201\n")
        output = io.StringIO()
        with redirect_stdout(output):
            exec(workload["code"], {})
        self.assertEqual(output.getvalue(), workload["expected_stdout"])
        self.assertNotIn("while True", workload["code"])

    def test_unready_executor_is_not_mistaken_for_quota_protection(self):
        plot.validate_readiness([{"http_status": 200, "status": {"ready": True}}])
        for bad in ([], [{"error": "disconnected"}], [{"http_status": 200, "status": {"ready": False}}]):
            with self.assertRaises(ValueError):
                plot.validate_readiness(bad)

    def test_invalid_sources_prevent_successful_experiment(self):
        report = dict(core_offered=100, core_succeeded=100, core_errors=0, observation_errors=0, sandbox_errors=0,
                      workload_checksums=["abc"], sandbox_requests=0, sandbox_completed=0,
                      sample_quality={"usable_frames": 0, "unavailable_frames": 0})
        with self.assertRaises(RuntimeError):
            check_phase(report, True, 20, 5)


if __name__ == "__main__":
    unittest.main()
