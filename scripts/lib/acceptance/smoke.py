"""Manage one owned kind functional lab; never uses the caller's kubeconfig."""

#!/usr/bin/env python3

import json
import time
import uuid

from lib.core.paths import CLUSTER, NAMESPACE, OWNER_LABEL
from lib.core.errors import LabError
from lib.core.state import atomic_json
from lib.manifests import render as render_manifest

class SmokeAcceptance:
    def __init__(self, lab):
        self.lab = lab

    def namespace(self, state):
        output = self.lab.kubectl("get", "namespace", NAMESPACE, "--ignore-not-found", "-o", "json")
        if output.strip():
            obj = json.loads(output)
            if obj["metadata"].get("labels", {}).get(OWNER_LABEL) != state["session"]:
                raise LabError("unowned_namespace", "refusing to use another session's namespace")
        else:
            obj = render_manifest(self.lab.config, 'platform-namespace', {'NAME': NAMESPACE, 'OWNER': state['session']})
            self.lab.kubectl("create", "-f", "-", input_text=json.dumps(obj))

    def test(self, source=None, timeout=180):
        image = self.lab.python_image(source)
        with self.lab.operation_lock():
            state, _ = self.lab.owned(complete=True)
            nodes = self.lab.node_report()
            self.namespace(state)
            run_id = uuid.uuid4().hex[:12]
            name = "agentbox-smoke-" + run_id
            obj = render_manifest(self.lab.config, 'smoke', {'OWNER': state['session'], 'RUN_ID': run_id, 'SMOKE_IMAGE': image})
            manifest = json.dumps(obj)
            (self.lab.runner.output / "smoke-job.json").write_text(manifest)
            self.lab.kubectl("-n", NAMESPACE, "create", "-f", "-", input_text=manifest)
            print(f"[platform] awaiting Job {NAMESPACE}/{name} (budget {timeout}s)", flush=True)
            deadline = time.monotonic() + timeout
            while True:
                job = json.loads(self.lab.kubectl("-n", NAMESPACE, "get", "job", name, "-o", "json", announce=False))
                if job["metadata"].get("labels", {}).get(OWNER_LABEL) != state["session"]:
                    raise LabError("job_identity_changed", "Job ownership no longer matches this session")
                status = job.get("status", {})
                if any(c["type"] == "Failed" and c["status"] == "True" for c in status.get("conditions", [])):
                    raise LabError("job_failed", json.dumps(status))
                if status.get("succeeded") == 1 and any(
                        c["type"] == "Complete" and c["status"] == "True"
                        for c in status.get("conditions", [])):
                    break
                if time.monotonic() >= deadline:
                    raise LabError("job_timeout", "Job did not succeed within the smoke budget")
                time.sleep(2)
            pods = json.loads(self.lab.kubectl("-n", NAMESPACE, "get", "pods", "-l", "job-name=" + name, "-o", "json"))["items"]
            if len(pods) != 1:
                raise LabError("unexpected_attempts", "smoke requires exactly one Pod attempt")
            pod = pods[0]
            containers = pod["spec"].get("containers", [])
            if (pod["spec"].get("nodeName") != CLUSTER + "-worker" or
                    pod["status"].get("phase") != "Succeeded" or
                    pod["metadata"].get("labels", {}).get(OWNER_LABEL) != state["session"] or
                    len(containers) != 1 or containers[0].get("name") != "compute" or
                    containers[0].get("image") != image):
                raise LabError("pod_validation_failed", "wrong node, phase, session or pinned image")
            if not any(o.get("uid") == job["metadata"]["uid"] and o.get("kind") == "Job" and
                       o.get("controller") is True for o in pod["metadata"].get("ownerReferences", [])):
                raise LabError("pod_validation_failed", "Pod is not controlled by this Job UID")
            logs = self.lab.kubectl("-n", NAMESPACE, "logs", pod["metadata"]["name"], "-c", "compute", "--limit-bytes=4096")
            if json.loads(logs) != {"result": self.lab.lock["smoke"]["expected_result"]}:
                raise LabError("incorrect_result", "Job completed but output failed validation")
            result = {"job_uid": job["metadata"]["uid"], "pod_uid": pod["metadata"]["uid"],
                      "job": name, "pod": pod["metadata"]["name"], "node": pod["spec"]["nodeName"],
                      "image_source": image, "container_statuses": pod["status"].get("containerStatuses", []),
                      "output": logs, "nodes": nodes}
            atomic_json(self.lab.runner.output / "smoke.json", result)
            self.lab.kubectl("-n", NAMESPACE, "delete", "job", name, "--cascade=foreground",
                         "--wait=true", "--timeout=60s", timeout=80)
            cleanup_deadline = time.monotonic() + 60
            while True:
                remaining = json.loads(self.lab.kubectl("-n", NAMESPACE, "get", "pods", "-l",
                                                    "job-name=" + name, "-o", "json", announce=False))
                if not remaining.get("items"):
                    break
                if time.monotonic() >= cleanup_deadline:
                    raise LabError("cleanup_incomplete", "Job Pods did not disappear within the cleanup budget")
                time.sleep(1)
            result["cleanup_complete"] = True
            atomic_json(self.lab.runner.output / "smoke.json", result)
            return result

