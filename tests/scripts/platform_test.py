"""Ownership, isolation and failure regressions; no Docker or cluster required."""

from contextlib import redirect_stdout
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))
spec = importlib.util.spec_from_file_location("platform_lab", ROOT / "scripts/platform_lab.py")
labmod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(labmod)


class FakeRunner:
    def __init__(self, output):
        self.output = output
        self.calls = []
        self.handler = lambda args, **kwargs: ""

    def run(self, args, **kwargs):
        args = [str(x) for x in args]
        self.calls.append((args, kwargs))
        return self.handler(args, **kwargs)


class PlatformTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.config = self.root / "deploy/platform"
        self.config.mkdir(parents=True)
        for name in ("versions.lock.json", "kind.yaml", "smoke-job.yaml"):
            shutil.copyfile(ROOT / "deploy/platform" / name, self.config / name)
        self.output = self.root / "evidence"
        self.output.mkdir()
        self.runner = FakeRunner(self.output)
        self.lab = labmod.Lab(self.root, self.runner)

    def state(self, **changes):
        self.lab.state_dir.mkdir(parents=True, exist_ok=True)
        self.lab.kubeconfig.write_text("dedicated kubeconfig")
        value = dict(schema_version=1, project=str(self.root), cluster=labmod.CLUSTER,
                     session="a"*32, phase="ready", docker_id="docker-a", node_ids=["1"*64, "2"*64],
                     kubeconfig_sha256=labmod.file_hash(self.lab.kubeconfig),
                     lock_sha256=labmod.file_hash(self.config / "versions.lock.json"))
        value.update(changes)
        labmod.atomic_json(self.lab.state_path, value)
        return value

    def test_mirror_can_only_change_repository(self):
        canonical = self.lab.lock["smoke"]["image"]
        digest = canonical.split("@")[1]
        self.assertEqual(labmod.pinned_source(canonical, "mirror.example/python@"+digest),
                         "mirror.example/python@"+digest)
        for value in ("python:latest", "mirror/python@sha256:"+"0"*64, "https://mirror/image@"+digest):
            with self.subTest(value=value), self.assertRaises(labmod.LabError):
                labmod.pinned_source(canonical, value)

    def test_every_kubectl_call_has_dedicated_config_and_context(self):
        with patch.dict("os.environ", {"KUBECONFIG": "/unrelated/production-config"}):
            self.lab.kubectl("get", "nodes")
        args = self.runner.calls[0][0]
        self.assertEqual(args[1:5], ["--kubeconfig", str(self.lab.kubeconfig), "--context", labmod.CONTEXT])
        self.assertNotIn("/unrelated/production-config", args)

    def test_dry_run_neither_executes_nor_creates_state_or_output(self):
        destination = self.root / "must-not-exist"
        with patch.object(labmod, "ROOT", self.root), patch.object(labmod.Runner, "run", side_effect=AssertionError("executed")), redirect_stdout(io.StringIO()):
            self.assertEqual(labmod.main(["create", "--dry-run", "--output", str(destination)]), 0)
        self.assertFalse(destination.exists())
        self.assertFalse(self.lab.state_dir.exists())

    def test_create_refuses_existing_unowned_cluster(self):
        self.runner.handler = lambda args, **kw: labmod.CLUSTER+"\n" if "clusters" in args else ""
        with patch.object(self.lab, "preflight", return_value={"ok": True, "docker": {"ID": "docker-a"}}):
            with self.assertRaisesRegex(labmod.LabError, "refusing to adopt"):
                self.lab.create()
        self.assertFalse(any("create" in call[0] for call in self.runner.calls))
        self.assertFalse(self.lab.state_path.exists())

    def test_create_failure_keeps_partial_ownership_for_cleanup(self):
        def handler(args, **kwargs):
            if "create" in args:
                raise labmod.CommandError("command_timeout", "injected create timeout")
            return ""
        self.runner.handler = handler
        with patch.object(self.lab, "preflight", return_value={"ok": True, "docker": {"ID": "docker-a"}}), patch.object(self.lab, "prepare_image"), patch.object(self.lab, "node_ids", side_effect=[[], ["1"*64]]):
            with self.assertRaises(labmod.CommandError):
                self.lab.create()
        state = self.lab.read_state()
        self.assertEqual(state["phase"], "failed")
        self.assertEqual(state["node_ids"], ["1"*64])
        self.assertFalse(any("delete" in call[0] for call in self.runner.calls))
        self.assertIn("--retain", next(c[0] for c in self.runner.calls if "create" in c[0]))
        self.assertTrue(next(c[1]["clear_proxy"] for c in self.runner.calls if "create" in c[0]))

    def test_delete_requires_ownership_without_calling_docker(self):
        with self.assertRaises(labmod.LabError):
            self.lab.delete()
        self.assertEqual(self.runner.calls, [])

    def test_image_preparation_requires_digest_and_has_separate_pull_budget(self):
        image = self.lab.lock["kubernetes"]["node_image"]
        replies = [labmod.CommandError("command_failed", "not cached"), "", json.dumps([image])]
        def handler(args, **kwargs):
            reply = replies.pop(0)
            if isinstance(reply, Exception):
                raise reply
            return reply
        self.runner.handler = handler
        self.lab.prepare_image(image)
        self.assertEqual(self.runner.calls[1][0][1], "pull")
        self.assertEqual(self.runner.calls[1][1]["timeout"], 600)
        self.runner.handler = lambda args, **kwargs: json.dumps(["wrong@sha256:"+"0"*64])
        with self.assertRaises(labmod.LabError):
            self.lab.prepare_image(image)

    def test_delete_refuses_replaced_nodes_and_docker_daemon(self):
        self.state()
        for info, nodes, reason in [({"ID": "docker-b"}, ["1"*64], "docker_identity_changed"),
                                    ({"ID": "docker-a"}, ["3"*64], "node_identity_changed")]:
            with patch.object(self.lab, "docker_info", return_value=info), patch.object(self.lab, "node_ids", return_value=nodes):
                with self.assertRaises(labmod.LabError) as caught:
                    self.lab.delete()
                self.assertEqual(caught.exception.reason, reason)
        self.assertFalse(any("delete" in c[0] for c in self.runner.calls))

    def test_delete_refuses_modified_kubeconfig(self):
        self.state()
        self.lab.kubeconfig.write_text("somebody else's context")
        with patch.object(self.lab, "docker_info", return_value={"ID": "docker-a"}), patch.object(self.lab, "node_ids", return_value=["1"*64]):
            with self.assertRaises(labmod.LabError) as caught:
                self.lab.delete()
        self.assertEqual(caught.exception.reason, "kubeconfig_changed")

    def test_delete_allows_missing_owned_node_and_confirms_containers_gone(self):
        self.state()
        with patch.object(self.lab, "docker_info", return_value={"ID": "docker-a"}), patch.object(self.lab, "node_ids", side_effect=[["1"*64], []]):
            result = self.lab.delete()
        self.assertTrue(result["cleanup_complete"])
        self.assertFalse(self.lab.kubeconfig.exists())
        self.assertEqual(self.lab.read_state()["phase"], "deleted")
        self.assertEqual(self.runner.calls[0][0][1:5], ["delete", "cluster", "--name", labmod.CLUSTER])

    def test_partial_kind_delete_cannot_destroy_credentials_needed_for_retry(self):
        original = self.state()
        def failed_delete(args, **kwargs):
            scratch = Path(args[args.index("--kubeconfig")+1])
            self.assertNotEqual(scratch, self.lab.kubeconfig)
            scratch.write_text("kind removed its context")
            raise labmod.CommandError("command_failed", "one node did not exit")
        self.runner.handler = failed_delete
        with patch.object(self.lab, "docker_info", return_value={"ID": "docker-a"}), patch.object(self.lab, "node_ids", return_value=["1"*64]):
            with self.assertRaises(labmod.CommandError):
                self.lab.delete()
        self.assertEqual(self.lab.read_state()["phase"], "delete_failed")
        self.assertEqual(labmod.file_hash(self.lab.kubeconfig), original["kubeconfig_sha256"])
        self.runner.handler = lambda args, **kwargs: ""
        with patch.object(self.lab, "docker_info", return_value={"ID": "docker-a"}), patch.object(self.lab, "node_ids", side_effect=[["1"*64], []]):
            self.assertTrue(self.lab.delete()["cleanup_complete"])
        self.assertEqual(self.lab.read_state()["phase"], "deleted")

    def smoke_responses(self, output='{"result":333833500}', node=None, job_failed=False):
        session = "a"*32
        job = {"metadata": {"uid": "job-uid", "labels": {labmod.OWNER_LABEL: session}},
               "status": {"conditions": [{"type": "Failed", "status": "True"}]} if job_failed else
                         {"succeeded": 1, "conditions": [{"type": "Complete", "status": "True"}]}}
        pod = {"metadata": {"name": "smoke-pod", "uid": "pod-uid", "labels": {labmod.OWNER_LABEL: session},
                            "ownerReferences": [{"kind": "Job", "uid": "job-uid", "controller": True}]},
               "spec": {"nodeName": node or labmod.CLUSTER+"-worker",
                        "containers": [{"name": "compute", "image": self.lab.lock["smoke"]["image"]}]},
               "status": {"phase": "Succeeded"}}
        remaining = [pod]

        def kubectl(*args, **kwargs):
            if "create" in args:
                self.assertIn("image: "+self.lab.lock["smoke"]["image"], kwargs["input_text"])
                return ""
            if "get" in args and "job" in args:
                return json.dumps(job)
            if "get" in args and "pods" in args:
                return json.dumps({"items": list(remaining)})
            if "logs" in args:
                return output
            if "delete" in args:
                remaining.clear()
                return ""
            raise AssertionError(args)
        return {"session": session}, kubectl

    def test_success_requires_worker_correct_output_and_cleanup(self):
        state, handler = self.smoke_responses()
        with patch.object(self.lab, "owned", return_value=(state, [])), patch.object(self.lab, "node_report", return_value=[]), patch.object(self.lab, "namespace"), patch.object(self.lab, "kubectl", side_effect=handler):
            result = self.lab.smoke()
        self.assertTrue(result["cleanup_complete"])
        self.assertEqual(result["job_uid"], "job-uid")
        self.assertEqual(result["pod_uid"], "pod-uid")

    def test_smoke_waits_for_asynchronous_pod_cleanup(self):
        state, handler = self.smoke_responses()
        deleted = False
        cleanup_polls = 0
        def delayed(*args, **kwargs):
            nonlocal deleted, cleanup_polls
            if "delete" in args:
                deleted = True
            elif deleted and "get" in args and "pods" in args:
                cleanup_polls += 1
                if cleanup_polls == 1:
                    return json.dumps({"items": [{"metadata": {"name": "terminating"}}]})
            return handler(*args, **kwargs)
        with patch.object(self.lab, "owned", return_value=(state, [])), patch.object(self.lab, "node_report", return_value=[]), patch.object(self.lab, "namespace"), patch.object(self.lab, "kubectl", side_effect=delayed), patch.object(labmod.time, "sleep"):
            result = self.lab.smoke()
        self.assertTrue(result["cleanup_complete"])
        self.assertEqual(cleanup_polls, 2)

    def test_cleanup_timeout_does_not_report_success(self):
        state, handler = self.smoke_responses()
        deleted = False
        def stuck(*args, **kwargs):
            nonlocal deleted
            if "delete" in args:
                deleted = True
            elif deleted and "get" in args and "pods" in args:
                return json.dumps({"items": [{"metadata": {"name": "stuck"}}]})
            return handler(*args, **kwargs)
        with patch.object(self.lab, "owned", return_value=(state, [])), patch.object(self.lab, "node_report", return_value=[]), patch.object(self.lab, "namespace"), patch.object(self.lab, "kubectl", side_effect=stuck), patch.object(labmod.time, "monotonic", side_effect=[0, 0, 61]):
            with self.assertRaises(labmod.LabError) as caught:
                self.lab.smoke()
        self.assertEqual(caught.exception.reason, "cleanup_incomplete")

    def test_wrong_output_wrong_node_or_failed_job_never_passes(self):
        for options, reason in [({"output": '{"result":0}'}, "incorrect_result"),
                                ({"node": "other-worker"}, "pod_validation_failed"),
                                ({"job_failed": True}, "job_failed")]:
            state, handler = self.smoke_responses(**options)
            with self.subTest(options=options), patch.object(self.lab, "owned", return_value=(state, [])), patch.object(self.lab, "node_report", return_value=[]), patch.object(self.lab, "namespace"), patch.object(self.lab, "kubectl", side_effect=handler) as calls:
                with self.assertRaises(labmod.LabError) as caught:
                    self.lab.smoke()
                self.assertEqual(caught.exception.reason, reason)
                self.assertFalse(any("delete" in c.args for c in calls.call_args_list))

    def test_preflight_reports_missing_tool_and_timeout_separately(self):
        def handler(args, **kwargs):
            if args[0] == self.lab.tools["kind"]:
                raise labmod.CommandError("tool_unavailable", "missing kind")
            raise labmod.CommandError("command_timeout", "timed out")
        self.runner.handler = handler
        result = self.lab.preflight()
        checks = {c["name"]: c for c in result["checks"]}
        self.assertFalse(result["ok"])
        self.assertEqual(checks["kind"]["reason"], "tool_unavailable")
        self.assertEqual(checks["kubectl"]["reason"], "command_timeout")
        self.assertEqual(checks["docker"]["reason"], "command_timeout")

    def test_busy_operation_does_not_probe_or_export_other_operation(self):
        destination = self.root / "busy-report"
        with self.lab.operation_lock(), patch.object(labmod, "ROOT", self.root), patch.object(labmod.Lab, "diagnostics", side_effect=AssertionError("interfered")), redirect_stdout(io.StringIO()):
            self.assertEqual(labmod.main(["create", "--output", str(destination)]), 1)
        report = json.loads((destination / "report.json").read_text())
        self.assertEqual(report["reason"], "operation_in_progress")
        self.assertEqual(report["commands"], [])

    def test_runner_kills_timed_out_command_and_classifies_failure(self):
        runner = labmod.Runner(self.output)
        with redirect_stdout(io.StringIO()), self.assertRaises(labmod.CommandError) as caught:
            runner.run([sys.executable, "-c", "import time; time.sleep(30)"], timeout=.05)
        self.assertEqual(caught.exception.reason, "command_timeout")
        self.assertIsNotNone(runner.commands[0]["exit_code"])

    def test_kind_child_proxy_is_cleared_without_modifying_parent_environment(self):
        runner = labmod.Runner(self.output)
        proxy = "http://127.0.0.1:7890"
        with patch.dict("os.environ", {"HTTP_PROXY": proxy}), redirect_stdout(io.StringIO()):
            output = runner.run([sys.executable, "-c",
                                 "import os,json; print(json.dumps([os.environ.get('HTTP_PROXY'),os.environ.get('NO_PROXY')]))"],
                                clear_proxy=True)
            self.assertEqual(json.loads(output), ["", "*"])
            self.assertEqual(labmod.os.environ["HTTP_PROXY"], proxy)
        self.assertEqual(runner.commands[0]["node_proxy_forwarding"], "disabled")

    def test_valid_version_with_wrong_binary_checksum_fails_preflight(self):
        files = {}
        for name in ("kind", "kubectl"):
            executable = self.root / name
            executable.write_text("test executable")
            self.lab.tools[name] = str(executable)
            files[name] = executable
        def handler(args, **kwargs):
            if args[0] == str(files["kind"]):
                return "kind "+self.lab.lock["kind"]["version"]
            if args[0] == str(files["kubectl"]):
                return json.dumps({"clientVersion": {"gitVersion": self.lab.lock["kubectl"]["version"]}})
            return json.dumps({"ID": "test-docker", "ServerVersion": "28.1.1", "OSType": "linux",
                               "Architecture": "x86_64", "CgroupVersion": "2", "CgroupDriver": "systemd",
                               "NCPU": 8, "MemTotal": 8 << 30})
        self.runner.handler = handler
        result = self.lab.preflight()
        checks = {c["name"]: c for c in result["checks"]}
        self.assertTrue(checks["kind"]["ok"])
        self.assertFalse(checks["kind_checksum"]["ok"])
        self.assertEqual(checks["kubectl_checksum"]["reason"], "checksum_mismatch")
        self.assertFalse(result["ok"])

    def test_output_directory_is_never_overwritten(self):
        with patch.object(labmod, "ROOT", self.root), self.assertRaises(FileExistsError):
            labmod.main(["preflight", "--output", str(self.output)])

    def test_failed_preflight_has_nonzero_exit_and_clear_summary(self):
        destination = self.root / "preflight-failed"
        result = {"ok": False, "checks": [{"name": "kind", "ok": False, "reason": "version_mismatch"}]}
        captured = io.StringIO()
        with patch.object(labmod, "ROOT", self.root), patch.object(labmod.Lab, "preflight", return_value=result), redirect_stdout(captured):
            self.assertEqual(labmod.main(["preflight", "--output", str(destination)]), 1)
        summary = json.loads(captured.getvalue())
        self.assertEqual(summary["reason"], "preflight_failed")
        self.assertEqual(summary["failed_checks"], [{"name": "kind", "reason": "version_mismatch"}])


if __name__ == "__main__":
    unittest.main()
