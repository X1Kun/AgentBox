"""Owned kind lifecycle, readiness and cluster identity checks."""

from lib.core.locking import OperationLock

import json
import os
from pathlib import Path
import platform
import re
import shutil
import sys
import tempfile
import time
import uuid

from lib.core.paths import CLUSTER, CONTEXT, HEX64
from lib.core.errors import LabError, CommandError
from lib.core.state import utc, file_hash, atomic_json
from lib.manifests import load_lock, lock_identity, python_image_source

def pinned_source(canonical, supplied):
    """Mirrors may change repository only, never the expected content digest."""
    value = supplied or canonical
    if "://" in value or not re.fullmatch(r"[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}", value):
        raise LabError("invalid_image", "image source must be a digest-pinned repository")
    if value.rsplit("@", 1)[1] != canonical.rsplit("@", 1)[1]:
        raise LabError("image_digest_mismatch", "mirror digest differs from versions.lock.yaml")
    return value


class Lab:
    def __init__(self, root, runner):
        self.root = Path(root).resolve()
        self.runner = runner
        self.config = self.root / "deploy"
        self.state_dir = self.root / ".local/platform"
        self._operation_lock = OperationLock(self.state_dir / "operation.lock")
        self.state_path = self.state_dir / "cluster.json"
        self.kubeconfig = self.state_dir / "kubeconfig"
        self.lock = load_lock(self.config, 'versions')
        if self.lock.get("schema_version") != 1:
            raise LabError("invalid_lock", "unsupported versions.lock.yaml schema")
        for name in ("kind", "kubectl"):
            if not HEX64.fullmatch(self.lock[name].get("sha256", "")):
                raise LabError("invalid_lock", "tool checksum is missing")
        for image in (self.lock["kubernetes"]["node_image"], self.lock["smoke"]["image"]):
            pinned_source(image, None)
        self.tools = {name: self.tool(name) for name in ("kind", "kubectl", "docker")}

    write_json = staticmethod(atomic_json)

    def write_state(self, state):
        atomic_json(self.state_path, state)

    def tool(self, name):
        override = os.environ.get("AGENTBOX_" + name.upper())
        local = self.root / ".local/platform-tools" / name
        return override or (str(local) if local.is_file() else shutil.which(name) or name)

    @staticmethod
    def transient_read_error(error):
        message = str(error).lower()
        if any(x in message for x in ("x509", "certificate", "unauthorized", "forbidden", "credential")):
            return False
        return error.reason == "command_timeout" or (error.reason == "command_failed" and any(
            x in message for x in ("deadline exceeded", "timed out", "timeout", "connection refused",
                "connection reset", "unable to connect", "serviceunavailable", "internalerror", "too many requests", " eof")))

    def kubectl(self, *args, timeout=30, input_text=None, announce=True, request_timeout=15):
        # Retry only explicit GET/version operations within the original total
        # subprocess budget. Never replay create/apply/patch/delete/exec.
        read_only = bool(args) and (args[0] in ("get", "version") or
            (len(args) >= 3 and args[0] in ("-n", "--namespace") and args[2] == "get"))
        deadline = time.monotonic() + timeout
        attempts = 0
        while True:
            remaining = max(.001, deadline - time.monotonic())
            budget = round(max(.001, min(request_timeout, remaining)), 3) if attempts else request_timeout
            try:
                return self.runner.run([self.tools["kubectl"], "--kubeconfig", self.kubeconfig,
                    "--context", CONTEXT, f"--request-timeout={budget}s", *args],
                    timeout=remaining, input_text=input_text, announce=announce)
            except CommandError as error:
                if not read_only or not self.transient_read_error(error) or deadline - time.monotonic() <= 1:
                    raise
                attempts += 1
                print("[platform] transient Kubernetes read failure; retrying within the same budget", flush=True)
                time.sleep(1)

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

    def operation_lock(self):
        return self._operation_lock.hold()

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
                         lock_identity(self.config, "versions")):
            raise LabError("baseline_changed", "kubeconfig is missing or the version lock changed; recreate the lab")
        if complete and state.get("kind_config_sha256"):
            config = self.config / ("lab/kind-calico.yaml" if state.get("network_profile") == "calico" else "lab/kind.yaml")
            if file_hash(config) != state["kind_config_sha256"]:
                raise LabError("baseline_changed", "kind configuration changed; recreate the lab")
        return state, actual

    def node_report(self, require_ready=True, data=None):
        if data is None:
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
            if ((require_ready and not ready) or info["kubeletVersion"] != self.lock["kubernetes"]["version"] or
                    not info["containerRuntimeVersion"].startswith("containerd://")):
                raise LabError("node_baseline_mismatch", "node readiness/version/runtime differs from the baseline")
            result.append({"name": node["metadata"]["name"], "uid": node["metadata"]["uid"],
                           "labels": node["metadata"].get("labels", {}), "node_info": info})
        worker = next(n for n in result if n["name"].endswith("-worker"))
        if worker["labels"].get("agentbox.dev/lab") != CLUSTER or worker["labels"].get("agentbox.dev/role") != "worker":
            raise LabError("worker_labels_mismatch", "dedicated worker labels are missing")
        return result

    def wait_ready(self, timeout=180, require_nodes=True):
        """Bounded read-only convergence; no replay of writes or component restart."""
        state, actual = self.owned()
        if (len(actual) != 2 or actual != state.get("node_ids") or
                state.get("phase") not in ("network_pending", "ready")):
            raise LabError("cluster_not_ready", "finish owned cluster creation before health observation")
        deadline = time.monotonic() + timeout
        attempts, consecutive = [], 0
        expected = {"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}
        def read(*args):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise LabError("control_plane_not_ready", "health observation deadline reached")
            budget = round(min(5, remaining), 3)
            return self.kubectl(*args, request_timeout=budget, timeout=budget + 1, announce=False)
        print("[platform] waiting for API/etcd, native control plane" +
              (" and Nodes" if require_nodes else " (CNI pending)") + f"; budget {timeout}s", flush=True)
        while time.monotonic() < deadline:
            sample = {"elapsed_seconds": round(timeout - (deadline - time.monotonic()), 3)}
            try:
                api = read("get", "--raw=/readyz").strip() == "ok"
                node_data = json.loads(read("get", "nodes", "-o", "json"))
                self.node_report(require_ready=False, data=node_data)
                objects = json.loads(read("-n", "kube-system", "get", "pods", "-l",
                    "component in (etcd,kube-apiserver,kube-controller-manager,kube-scheduler)",
                    "--field-selector", "spec.nodeName=" + CLUSTER + "-control-plane", "-o", "json"))["items"]
                components = [{"component": p["metadata"].get("labels", {}).get("component"),
                    "ready": not p["metadata"].get("deletionTimestamp") and
                         p.get("status", {}).get("phase") == "Running" and any(
                         c.get("type") == "Ready" and c.get("status") == "True"
                         for c in p.get("status", {}).get("conditions", []))} for p in objects]
                # node_report already validates original names, versions and Worker labels.
                nodes_ready = all(any(c.get("type") == "Ready" and c.get("status") == "True"
                    for c in p.get("status", {}).get("conditions", [])) for p in node_data["items"])
                healthy = api and len(components) == 4 and {c["component"] for c in components} == expected and all(c["ready"] for c in components)
                sample.update(api_ready=api, nodes_ready=nodes_ready, components=components)
                consecutive = consecutive + 1 if healthy and (nodes_ready or not require_nodes) else 0
            except CommandError as error:
                if not self.transient_read_error(error):
                    raise
                consecutive = 0
                sample["reason"] = "api_temporarily_unavailable"
            attempts.append(sample)
            value = {"ready": consecutive >= 3, "required_consecutive_samples": 3,
                     "budget_seconds": timeout, "attempts": attempts}
            atomic_json(self.runner.output / "control-plane-readiness.json", value)
            if value["ready"]:
                return value
            remaining = deadline - time.monotonic()
            if remaining > 0:
                time.sleep(min(2, remaining))
        raise LabError("control_plane_not_ready", "API/etcd, native control plane or Nodes did not stabilize; see control-plane-readiness.json")

    def up(self, calico_source=None, calico_prefix=None, gvisor_archive=None, session_source=None, node_source=None):
        """Install/resume one owned lab in dependency order, retaining healthy stages."""
        from lib.platform.calico import Network
        from lib.platform.gvisor import GVisor
        from lib.platform.sandbox import Sessions
        stages = []
        network, runtime, sessions = Network(self), GVisor(self), Sessions(self)
        calico_source = calico_source or self.root / ".local/platform-tools" / network.lock["manifest"]
        gvisor_archive = gvisor_archive or self.root / ".local/platform-tools" / runtime.lock["archive"]
        session_source = session_source or self.root / ".local/platform-tools" / sessions.lock["manifest"]
        # Avoid creating a cold cluster only to discover missing local assets.
        for asset in (calico_source, gvisor_archive, session_source, sessions.bundle_path):
            if not Path(asset).is_file():
                raise LabError("missing_bootstrap_asset", "prepare pinned platform assets and run session-build before up")
        with self.operation_lock():
            state = self.read_state() if self.state_path.exists() else None
            if state is None or state.get("phase") == "deleted":
                self.create(node_source, "calico")
            else:
                state, _ = self.owned()
                if state.get("network_profile") != "calico" or state.get("phase") not in ("network_pending", "ready"):
                    raise LabError("cluster_not_ready", "up only resumes an owned Calico lab after successful create")
            def stage(name, operation):
                print("[platform] bootstrap stage: " + name, flush=True)
                result = operation()
                stages.append(name)
                atomic_json(self.runner.output / "bootstrap.json", {"completed_stages": stages})
                return result
            stage("control-plane", lambda: self.wait_ready(require_nodes=self.read_state().get("phase") == "ready"))
            stage("calico", lambda: network.install(calico_source, calico_prefix))
            stage("nodes", self.wait_ready)
            stage("gvisor", lambda: runtime.install(gvisor_archive))
            stage("control-plane-after-runtime", self.wait_ready)
            stage("sessions", lambda: sessions.install(session_source))
            return {"completed_stages": stages, "cluster_retained": True}

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

    def create(self, source=None, network_profile="kindnet"):
        from lib.platform.calico import Network
        image = pinned_source(self.lock["kubernetes"]["node_image"], source)
        if network_profile not in ("kindnet", "calico"):
            raise LabError("invalid_network_profile", "select kindnet or calico")
        kind_config = self.config / ("lab/kind-calico.yaml" if network_profile == "calico" else "lab/kind.yaml")
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
                     "lock_sha256": lock_identity(self.config, "versions"),
                     "network_profile": network_profile, "kind_config_sha256": file_hash(kind_config)}
            if network_profile == "calico":
                state["network_definition"] = Network(self).definition()
            atomic_json(self.state_path, state)
            try:
                self.prepare_image(image)
                state["phase"] = "creating"
                atomic_json(self.state_path, state)
                shutil.copyfile(kind_config, self.runner.output / "kind.yaml")
                self.runner.run([self.tools["kind"], "create", "cluster", "--name", CLUSTER,
                                 "--config", kind_config, "--image", image,
                                 "--kubeconfig", self.kubeconfig, "--retain", "--wait",
                                 "0s" if network_profile == "calico" else "180s"],
                                timeout=420, clear_proxy=True)
                self.kubeconfig.chmod(0o600)
                if network_profile == "kindnet":
                    self.kubectl("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=120s", timeout=140)
                state["kubernetes_version"] = json.loads(self.kubectl("version", "-o", "json"))
                if state["kubernetes_version"].get("serverVersion", {}).get("gitVersion") != self.lock["kubernetes"]["version"]:
                    raise LabError("server_version_mismatch", "API server version differs from the locked baseline")
                state["nodes"] = self.node_report(require_ready=network_profile == "kindnet")
                state["phase"] = "network_pending" if network_profile == "calico" else "ready"
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

    def python_image(self, source=None):
        return pinned_source(self.lock["smoke"]["image"], source or python_image_source(self.config))

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

