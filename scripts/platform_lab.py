#!/usr/bin/env python3
"""Manage one owned kind functional lab; never uses the caller's kubeconfig."""

import argparse
import contextlib
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
CLUSTER = "agentbox-platform-lab"
CONTEXT = "kind-" + CLUSTER
NAMESPACE = "agentbox-platform"
OWNER_LABEL = "agentbox.dev/session"
HEX64 = re.compile(r"^[0-9a-f]{64}$")


class LabError(Exception):
    def __init__(self, reason, detail):
        super().__init__(detail)
        self.reason = reason


class CommandError(LabError):
    pass


def utc():
    return datetime.now(timezone.utc).isoformat()


def file_hash(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def atomic_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def pinned_source(canonical, supplied):
    """Mirrors may change repository only, never the expected content digest."""
    value = supplied or canonical
    if "://" in value or not re.fullmatch(r"[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}", value):
        raise LabError("invalid_image", "image source must be a digest-pinned repository")
    if value.rsplit("@", 1)[1] != canonical.rsplit("@", 1)[1]:
        raise LabError("image_digest_mismatch", "mirror digest differs from versions.lock.json")
    return value


class Runner:
    def __init__(self, output):
        self.output = output
        self.commands = []

    def run(self, args, timeout=30, input_text=None, announce=True, clear_proxy=False):
        args = [str(x) for x in args]
        if announce:
            print("[platform] " + " ".join(args), flush=True)
        begin = time.monotonic()
        record = {"argv": args, "started_at": utc()}
        self.commands.append(record)
        env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER="docker")
        if clear_proxy:
            # A host loopback proxy is unreachable inside a kind node.
            # Only this child environment changes; never edit host settings.
            for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
                env[key] = ""
            env["NO_PROXY"] = env["no_proxy"] = "*"
            record["node_proxy_forwarding"] = "disabled"
        try:
            proc = subprocess.Popen(args, stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                    env=env, start_new_session=True)
        except OSError as error:
            record.update(reason="tool_unavailable", detail=str(error))
            raise CommandError("tool_unavailable", f"{args[0]}: {error}") from error
        try:
            stdout, stderr = proc.communicate(input_text, timeout=timeout)
        except (subprocess.TimeoutExpired, KeyboardInterrupt) as error:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(proc.pid, signal.SIGKILL)
            stdout, stderr = proc.communicate()
            if isinstance(error, KeyboardInterrupt):
                record["reason"] = "command_interrupted"
                raise
            record.update(reason="command_timeout", timeout_seconds=timeout)
            raise CommandError("command_timeout", f"{Path(args[0]).name} exceeded {timeout}s")
        finally:
            record["elapsed_seconds"] = round(time.monotonic() - begin, 3)
            if "stdout" in locals():
                record.update(exit_code=proc.returncode, stdout=stdout[-65536:], stderr=stderr[-65536:])
        if proc.returncode:
            raise CommandError("command_failed",
                               f"{Path(args[0]).name} exited {proc.returncode}: {stderr[-2000:].strip()}")
        return stdout


class Lab:
    def __init__(self, root, runner):
        self.root = Path(root).resolve()
        self.runner = runner
        self.config = self.root / "deploy/platform"
        self.state_dir = self.root / ".local/platform"
        self.state_path = self.state_dir / "cluster.json"
        self.kubeconfig = self.state_dir / "kubeconfig"
        self.lock = json.loads((self.config / "versions.lock.json").read_text())
        if self.lock.get("schema_version") != 1:
            raise LabError("invalid_lock", "unsupported versions.lock.json schema")
        for name in ("kind", "kubectl"):
            if not HEX64.fullmatch(self.lock[name].get("sha256", "")):
                raise LabError("invalid_lock", "tool checksum is missing")
        for image in (self.lock["kubernetes"]["node_image"], self.lock["smoke"]["image"]):
            pinned_source(image, None)
        self.tools = {name: self.tool(name) for name in ("kind", "kubectl", "docker")}

    def tool(self, name):
        override = os.environ.get("AGENTBOX_" + name.upper())
        local = self.root / ".local/platform-tools" / name
        return override or (str(local) if local.is_file() else shutil.which(name) or name)

    def kubectl(self, *args, timeout=30, input_text=None, announce=True):
        return self.runner.run([self.tools["kubectl"], "--kubeconfig", self.kubeconfig,
                                "--context", CONTEXT, "--request-timeout=15s", *args],
                               timeout=timeout, input_text=input_text, announce=announce)

    def docker_info(self):
        try:
            data = json.loads(self.runner.run([self.tools["docker"], "info", "--format", "{{json .}}"]))
        except CommandError as error:
            reason = "docker_unreachable" if error.reason == "command_failed" else error.reason
            raise LabError(reason, str(error)) from error
        return {key: data.get(key) for key in ("ID", "ServerVersion", "OSType", "Architecture",
                                              "CgroupVersion", "CgroupDriver", "NCPU", "MemTotal")}

    def preflight(self):
        checks = []

        def add(name, ok, observed, reason="requirement_failed"):
            checks.append({"name": name, "ok": bool(ok), "observed": observed,
                           "reason": "ok" if ok else reason})

        add("host_platform", platform.system() == "Linux" and platform.machine() == "x86_64",
            platform.system() + "/" + platform.machine())
        add("python", sys.version_info >= (3, 10), platform.python_version())
        add("host_cgroup_v2", Path("/sys/fs/cgroup/cgroup.controllers").is_file(), "cgroup v2")
        for name in ("kind", "kubectl"):
            args = [self.tools[name], "version"]
            if name == "kubectl":
                args += ["--client", "-o", "json"]
            try:
                output = self.runner.run(args, timeout=15)
                version = (json.loads(output)["clientVersion"]["gitVersion"] if name == "kubectl"
                           else re.search(r"\bv\d+\.\d+\.\d+\b", output).group())
                add(name, version == self.lock[name]["version"], version, "version_mismatch")
                executable = shutil.which(self.tools[name])
                checksum = file_hash(executable) if executable else None
                add(name + "_checksum", checksum == self.lock[name]["sha256"],
                    checksum, "checksum_mismatch")
            except (LabError, OSError, ValueError, KeyError, AttributeError) as error:
                add(name, False, str(error), getattr(error, "reason", "invalid_version_output"))
        info = None
        try:
            info = self.docker_info()
            requirements = self.lock["docker"]
            add("docker_identity", isinstance(info["ID"], str) and bool(info["ID"]), info["ID"])
            add("docker_version", int(info["ServerVersion"].split(".")[0]) >= requirements["minimum_major"],
                info["ServerVersion"])
            add("docker_linux", info["OSType"] == "linux" and info["Architecture"] in ("x86_64", "amd64"),
                {"os": info["OSType"], "architecture": info["Architecture"]})
            add("docker_cgroup_v2", info["CgroupVersion"] == "2", info["CgroupVersion"])
            add("docker_capacity", info["NCPU"] >= requirements["minimum_cpus"] and
                info["MemTotal"] >= requirements["minimum_memory_bytes"],
                {"cpus": info["NCPU"], "memory_bytes": info["MemTotal"]})
        except (LabError, ValueError, TypeError, KeyError) as error:
            add("docker", False, str(error), getattr(error, "reason", "invalid_docker_info"))
        return {"ok": all(c["ok"] for c in checks), "checks": checks, "docker": info,
                "psi_available": {n: Path("/proc/pressure", n).is_file() for n in ("cpu", "memory", "io")},
                "kvm_visible": Path("/dev/kvm").exists(),
                "node_proxy_forwarding": "disabled for kind create; use digest-preserving registry sources",
                "scope": "kind functional baseline; KVM/host containerd/helm/k6 are not required"}

    @contextlib.contextmanager
    def operation_lock(self):
        self.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        if self.state_dir.is_symlink():
            raise LabError("invalid_state", "state directory must not be a symlink")
        with (self.state_dir / "operation.lock").open("a+") as stream:
            try:
                fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise LabError("operation_in_progress", "another platform operation holds the lock") from error
            yield

    def node_ids(self):
        output = self.runner.run([self.tools["docker"], "ps", "-aq", "--no-trunc",
                                  "--filter", "label=io.x-k8s.kind.cluster=" + CLUSTER])
        return sorted(output.split())

    def read_state(self):
        try:
            state = json.loads(self.state_path.read_text())
        except (OSError, ValueError) as error:
            raise LabError("unowned_cluster", "no valid local ownership record") from error
        if (state.get("schema_version") != 1 or state.get("project") != str(self.root) or
                state.get("cluster") != CLUSTER or not re.fullmatch(r"[0-9a-f]{32}", state.get("session", ""))):
            raise LabError("unowned_cluster", "ownership record does not match this project")
        return state

    def owned(self, complete=False):
        state = self.read_state()
        if state["docker_id"] != self.docker_info()["ID"]:
            raise LabError("docker_identity_changed", "ownership belongs to another Docker daemon")
        actual = self.node_ids()
        expected = state.get("node_ids", [])
        if any(not HEX64.fullmatch(x) for x in expected) or not set(actual).issubset(expected):
            raise LabError("node_identity_changed", "cluster contains nodes not recorded by this session")
        if complete and (state.get("phase") != "ready" or len(actual) != 2 or actual != expected):
            raise LabError("cluster_not_ready", "both original nodes and a ready ownership record are required")
        if self.kubeconfig.exists() and file_hash(self.kubeconfig) != state.get("kubeconfig_sha256"):
            raise LabError("kubeconfig_changed", "dedicated kubeconfig differs from the recorded file")
        if complete and (not self.kubeconfig.is_file() or state["lock_sha256"] !=
                         file_hash(self.config / "versions.lock.json")):
            raise LabError("baseline_changed", "kubeconfig is missing or the version lock changed; recreate the lab")
        return state, actual

    def node_report(self):
        data = json.loads(self.kubectl("get", "nodes", "-o", "json"))
        items = data["items"]
        expected = {CLUSTER + "-control-plane", CLUSTER + "-worker"}
        if {n["metadata"]["name"] for n in items} != expected:
            raise LabError("unexpected_nodes", "expected exactly the control-plane and worker nodes")
        result = []
        for node in items:
            info = node["status"]["nodeInfo"]
            ready = any(c["type"] == "Ready" and c["status"] == "True"
                        for c in node["status"].get("conditions", []))
            if (not ready or info["kubeletVersion"] != self.lock["kubernetes"]["version"] or
                    not info["containerRuntimeVersion"].startswith("containerd://")):
                raise LabError("node_baseline_mismatch", "node readiness/version/runtime differs from the baseline")
            result.append({"name": node["metadata"]["name"], "uid": node["metadata"]["uid"],
                           "labels": node["metadata"].get("labels", {}), "node_info": info})
        worker = next(n for n in result if n["name"].endswith("-worker"))
        if worker["labels"].get("agentbox.dev/lab") != CLUSTER or worker["labels"].get("agentbox.dev/role") != "worker":
            raise LabError("worker_labels_mismatch", "dedicated worker labels are missing")
        return result

    def prepare_image(self, image):
        args = [self.tools["docker"], "image", "inspect", image, "--format", "{{json .RepoDigests}}"]
        try:
            output = self.runner.run(args)
        except CommandError:
            # Keep image transfer separate from the cluster bootstrap budget.
            self.runner.run([self.tools["docker"], "pull", "--platform", "linux/amd64", image], timeout=600)
            output = self.runner.run(args)
        digests = json.loads(output) or []
        if not any(d.endswith("@" + image.rsplit("@", 1)[1]) for d in digests):
            raise LabError("image_digest_mismatch", "Docker did not confirm the requested repository digest")

    def create(self, source=None):
        image = pinned_source(self.lock["kubernetes"]["node_image"], source)
        with self.operation_lock():
            preflight = self.preflight()
            if not preflight["ok"]:
                raise LabError("preflight_failed", json.dumps(preflight["checks"]))
            previous = self.read_state() if self.state_path.exists() else None
            if previous and previous.get("phase") != "deleted":
                raise LabError("existing_state", "use smoke or delete; create never replaces an existing session")
            clusters = self.runner.run([self.tools["kind"], "get", "clusters"]).split()
            if CLUSTER in clusters or self.node_ids() or self.kubeconfig.exists():
                raise LabError("unowned_cluster", "refusing to adopt or replace an existing cluster/kubeconfig")
            state = {"schema_version": 1, "project": str(self.root), "cluster": CLUSTER,
                     "session": uuid.uuid4().hex, "phase": "preparing_image", "created_at": utc(),
                     "docker_id": preflight["docker"]["ID"], "node_ids": [], "node_image_source": image,
                     "lock_sha256": file_hash(self.config / "versions.lock.json")}
            atomic_json(self.state_path, state)
            try:
                self.prepare_image(image)
                state["phase"] = "creating"
                atomic_json(self.state_path, state)
                shutil.copyfile(self.config / "kind.yaml", self.runner.output / "kind.yaml")
                self.runner.run([self.tools["kind"], "create", "cluster", "--name", CLUSTER,
                                 "--config", self.config / "kind.yaml", "--image", image,
                                 "--kubeconfig", self.kubeconfig, "--retain", "--wait", "180s"],
                                timeout=420, clear_proxy=True)
                self.kubeconfig.chmod(0o600)
                self.kubectl("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=120s", timeout=140)
                state["kubernetes_version"] = json.loads(self.kubectl("version", "-o", "json"))
                if state["kubernetes_version"].get("serverVersion", {}).get("gitVersion") != self.lock["kubernetes"]["version"]:
                    raise LabError("server_version_mismatch", "API server version differs from the locked baseline")
                state["nodes"] = self.node_report()
                state["phase"] = "ready"
            except BaseException:
                state["phase"] = "failed"
                raise
            finally:
                state["node_ids"] = self.node_ids()
                if self.kubeconfig.exists():
                    self.kubeconfig.chmod(0o600)
                    state["kubeconfig_sha256"] = file_hash(self.kubeconfig)
                atomic_json(self.state_path, state)
            return {"state": state, "preflight": preflight}

    def namespace(self, state):
        output = self.kubectl("get", "namespace", NAMESPACE, "--ignore-not-found", "-o", "json")
        if output.strip():
            obj = json.loads(output)
            if obj["metadata"].get("labels", {}).get(OWNER_LABEL) != state["session"]:
                raise LabError("unowned_namespace", "refusing to use another session's namespace")
        else:
            obj = {"apiVersion": "v1", "kind": "Namespace", "metadata": {
                "name": NAMESPACE, "labels": {OWNER_LABEL: state["session"],
                                             "app.kubernetes.io/managed-by": "agentbox-platform"}}}
            self.kubectl("create", "-f", "-", input_text=json.dumps(obj))

    def smoke(self, source=None, timeout=180):
        image = pinned_source(self.lock["smoke"]["image"], source)
        with self.operation_lock():
            state, _ = self.owned(complete=True)
            nodes = self.node_report()
            self.namespace(state)
            run_id = uuid.uuid4().hex[:12]
            name = "agentbox-smoke-" + run_id
            manifest = (self.config / "smoke-job.yaml").read_text()
            for key, value in {"__RUN_ID__": run_id, "__SESSION__": state["session"],
                               "__SMOKE_IMAGE__": image}.items():
                manifest = manifest.replace(key, value)
            (self.runner.output / "smoke-job.yaml").write_text(manifest)
            self.kubectl("-n", NAMESPACE, "create", "-f", "-", input_text=manifest)
            print(f"[platform] awaiting Job {NAMESPACE}/{name} (budget {timeout}s)", flush=True)
            deadline = time.monotonic() + timeout
            while True:
                job = json.loads(self.kubectl("-n", NAMESPACE, "get", "job", name, "-o", "json", announce=False))
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
            pods = json.loads(self.kubectl("-n", NAMESPACE, "get", "pods", "-l", "job-name=" + name, "-o", "json"))["items"]
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
            logs = self.kubectl("-n", NAMESPACE, "logs", pod["metadata"]["name"], "-c", "compute", "--limit-bytes=4096")
            if json.loads(logs) != {"result": self.lock["smoke"]["expected_result"]}:
                raise LabError("incorrect_result", "Job completed but output failed validation")
            result = {"job_uid": job["metadata"]["uid"], "pod_uid": pod["metadata"]["uid"],
                      "job": name, "pod": pod["metadata"]["name"], "node": pod["spec"]["nodeName"],
                      "image_source": image, "container_statuses": pod["status"].get("containerStatuses", []),
                      "output": logs, "nodes": nodes}
            atomic_json(self.runner.output / "smoke.json", result)
            self.kubectl("-n", NAMESPACE, "delete", "job", name, "--cascade=foreground",
                         "--wait=true", "--timeout=60s", timeout=80)
            cleanup_deadline = time.monotonic() + 60
            while True:
                remaining = json.loads(self.kubectl("-n", NAMESPACE, "get", "pods", "-l",
                                                    "job-name=" + name, "-o", "json", announce=False))
                if not remaining.get("items"):
                    break
                if time.monotonic() >= cleanup_deadline:
                    raise LabError("cleanup_incomplete", "Job Pods did not disappear within the cleanup budget")
                time.sleep(1)
            result["cleanup_complete"] = True
            atomic_json(self.runner.output / "smoke.json", result)
            return result

    def delete(self):
        with self.operation_lock():
            state, nodes = self.owned()
            state["phase"] = "deleting"
            atomic_json(self.state_path, state)
            try:
                if nodes:
                    # kind may remove kubeconfig entries even if node deletion
                    # fails. Keep the real credential file stable for retries.
                    with tempfile.TemporaryDirectory(prefix="delete-", dir=self.state_dir) as temporary:
                        scratch = Path(temporary) / "kubeconfig"
                        atomic_json(scratch, {"apiVersion": "v1", "kind": "Config",
                                             "clusters": [], "contexts": [], "users": [],
                                             "current-context": ""})
                        self.runner.run([self.tools["kind"], "delete", "cluster", "--name", CLUSTER,
                                         "--kubeconfig", scratch], timeout=180)
                if self.node_ids():
                    raise LabError("cleanup_incomplete", "kind node containers remain")
                if self.kubeconfig.exists() and file_hash(self.kubeconfig) != state.get("kubeconfig_sha256"):
                    raise LabError("kubeconfig_changed", "credential file changed during deletion; preserved for inspection")
                self.kubeconfig.unlink(missing_ok=True)
            except BaseException:
                state["phase"] = "delete_failed"
                atomic_json(self.state_path, state)
                raise
            state["phase"], state["deleted_at"] = "deleted", utc()
            atomic_json(self.state_path, state)
            return {"cluster": CLUSTER, "session": state["session"], "cleanup_complete": True}

    def diagnostics(self):
        """Best effort, only after checking Docker ownership. Never dumps kubeconfig."""
        try:
            state, nodes = self.owned()
            if nodes:
                for node in nodes:
                    try:
                        output = self.runner.run([self.tools["docker"], "exec", node, "ps", "-eLo",
                                                  "pid,tid,stat,wchan:32,comm"], timeout=10)
                        (self.runner.output / ("processes-" + node[:12] + ".txt")).write_text(output)
                    except LabError:
                        pass
                self.runner.run([self.tools["kind"], "export", "logs", "--name", CLUSTER,
                                 self.runner.output / "kind-logs"], timeout=60)
        except (LabError, ValueError, OSError):
            return


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("preflight", "create", "smoke", "delete"))
    parser.add_argument("--dry-run", action="store_true", help="offline plan; no tools, writes or Docker calls")
    parser.add_argument("--output", type=Path, help="new evidence directory; never overwritten")
    parser.add_argument("--node-image-source", help="mirror repository with the SAME locked node digest (create)")
    parser.add_argument("--smoke-image-source", help="mirror repository with the SAME locked task digest (smoke)")
    parser.add_argument("--timeout", type=int, default=180, help="Job completion budget, 30..600 seconds")
    args = parser.parse_args(argv)
    if args.node_image_source and args.action != "create":
        parser.error("--node-image-source applies only to create")
    if args.smoke_image_source and args.action != "smoke":
        parser.error("--smoke-image-source applies only to smoke")
    if not 30 <= args.timeout <= 600:
        parser.error("--timeout must be 30..600 seconds")
    return args


def main(argv=None):
    args = arguments(argv)
    if args.dry_run:
        lock = json.loads((ROOT / "deploy/platform/versions.lock.json").read_text())
        print(json.dumps({"action": args.action, "cluster": CLUSTER, "context": CONTEXT,
                          "kubeconfig": str(ROOT / ".local/platform/kubeconfig"),
                          "node_image": pinned_source(lock["kubernetes"]["node_image"], args.node_image_source),
                          "smoke_image": pinned_source(lock["smoke"]["image"], args.smoke_image_source),
                          "mutates_cluster": args.action != "preflight", "dry_run": True}, indent=2))
        return 0
    output = args.output or ROOT / "artifacts" / ("platform-" + args.action + "-" + uuid.uuid4().hex[:12])
    output.mkdir(parents=True, exist_ok=False)
    runner = Runner(output)
    report = {"schema_version": 1, "action": args.action, "started_at": utc(), "ok": False,
              "implementation_sha256": file_hash(__file__), "python_version": platform.python_version(),
              "kernel": platform.release(),
              "scope": "trusted kind/containerd functional smoke; not sandbox or performance acceptance"}
    lab = None
    try:
        lab = Lab(ROOT, runner)
        report["versions_lock"] = lab.lock
        if args.action == "preflight":
            report["result"] = lab.preflight()
            report["ok"] = report["result"]["ok"]
            if not report["ok"]:
                report["reason"] = "preflight_failed"
                report["failed_checks"] = [{"name": c["name"], "reason": c["reason"]}
                                           for c in report["result"]["checks"] if not c["ok"]]
        elif args.action == "create":
            report["result"] = lab.create(args.node_image_source)
            report["ok"] = True
        elif args.action == "smoke":
            report["result"] = lab.smoke(args.smoke_image_source, args.timeout)
            report["ok"] = True
        else:
            report["result"] = lab.delete()
            report["ok"] = True
    except (LabError, OSError, ValueError, KeyError, KeyboardInterrupt) as error:
        reason = "interrupted" if isinstance(error, KeyboardInterrupt) else getattr(error, "reason", "invalid_state")
        report.update(reason=reason, detail=str(error))
        if lab and args.action in ("create", "smoke") and reason not in {
                "operation_in_progress", "existing_state", "unowned_cluster", "preflight_failed",
                "image_digest_mismatch", "invalid_image", "tool_unavailable"}:
            lab.diagnostics()
    finally:
        report.update(finished_at=utc(), commands=runner.commands)
        atomic_json(output / "report.json", report)
        print(json.dumps({"ok": report["ok"], "reason": report.get("reason"),
                          "failed_checks": report.get("failed_checks", []),
                          "report": str(output / "report.json")}), flush=True)
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (LabError, OSError, ValueError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
