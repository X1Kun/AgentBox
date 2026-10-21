#!/usr/bin/env python3
"""Validate the installed fixed service; --lifecycle also stops/restarts it."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import json
import http.client
import socket
import os
from pathlib import Path
import subprocess
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from systemd_common import load_config


UNIT = "agentbox.service"
STATE = Path("/var/lib/agentbox")


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


def systemctl(*arguments):
    return subprocess.run(["systemctl", *arguments], check=True, text=True, capture_output=True).stdout.strip()


def property_value(name):
    return systemctl("show", UNIT, "--property="+name, "--value")


def wait_until(predicate, description, timeout=10):
    end = time.monotonic()+timeout
    while time.monotonic() < end:
        if predicate():
            return
        time.sleep(0.05)
    raise ValueError("timed out waiting for "+description)


def populated(path):
    try:
        return "populated 1" in (path / "cgroup.events").read_text().splitlines()
    except FileNotFoundError:
        return False


def process_identity(pid):
    try:
        data = Path(f"/proc/{pid}/stat").read_text()
        return data[data.rfind(")")+2:].split()[19]
    except (FileNotFoundError, ProcessLookupError):
        return None


def process_snapshot(root):
    processes = {}
    for file in root.rglob("cgroup.procs"):
        try:
            for pid in file.read_text().split():
                identity = process_identity(pid)
                if identity is not None:
                    processes[pid] = identity
        except FileNotFoundError:
            pass
    return processes


class Smoke:
    def __init__(self):
        if os.geteuid() != 0:
            raise ValueError("host smoke checks require root to read the private token and inspect service resources")
        manifest = Path("/opt/agentbox/INSTALL.json")
        config_path = Path("/etc/agentbox.env")
        for path in (manifest, config_path):
            if path.is_symlink() or not path.is_file() or path.stat().st_uid != 0:
                raise ValueError("fixed service requires root-owned deployment and configuration")
        if config_path.stat().st_mode & 0o077:
            raise ValueError("API configuration must be private")
        self.manifest = json.loads(manifest.read_text())
        if self.manifest.get("format") != 1:
            raise ValueError("unrecognized deployment manifest")
        self.config = load_config(config_path)
        backend_path = Path("/etc/agentbox-gateway-daemon.env")
        if backend_path.exists():
            if backend_path.is_symlink() or not backend_path.is_file() or backend_path.stat().st_uid != 0 or backend_path.stat().st_mode & 0o077:
                raise ValueError("Gateway daemon override must be private and root-owned")
            effective = load_config(backend_path)
            for key in ("AGENTBOX_LISTEN", "AGENTBOX_API_TOKEN"):
                self.config[key] = effective[key]
        self.unix_socket = self.config["AGENTBOX_LISTEN"][5:] if self.config["AGENTBOX_LISTEN"].startswith("unix:") else None
        self.url = "http://"+self.config["AGENTBOX_LISTEN"]
        self.client = build_opener(ProxyHandler({}), NoRedirects())
        self.results = []
        self.runtime_components = []
        self.sandbox_limits = None
        self.lifecycle_checked = False

    def http(self, path, body=None, authenticate=True):
        data = json.dumps(body).encode() if body is not None else None
        if getattr(self, "unix_socket", None):
            connection = http.client.HTTPConnection("agentbox", timeout=50)
            connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            connection.sock.settimeout(50)
            headers = {"Content-Type": "application/json"} if data is not None else {}
            if authenticate:
                headers["Authorization"] = "Bearer "+self.config["AGENTBOX_API_TOKEN"]
            try:
                connection.sock.connect(self.unix_socket)
                connection.request("POST" if data is not None else "GET", path, body=data, headers=headers)
                response = connection.getresponse()
                return response.status, json.loads(response.read(8 << 20))
            finally:
                connection.close()
        request = Request(self.url+path, data=data)
        if authenticate:
            request.add_header("Authorization", "Bearer "+self.config["AGENTBOX_API_TOKEN"])
        if data is not None:
            request.add_header("Content-Type", "application/json")
        try:
            response = self.client.open(request, timeout=45)
        except HTTPError as error:
            response = error
        with response:
            return response.code, json.loads(response.read(8 << 20))

    def ready(self):
        try:
            code, body = self.http("/readyz", authenticate=False)
            return code == 200 and body.get("ready") is True
        except (OSError, URLError):
            return False

    def start(self):
        if property_value("ActiveState") == "failed":
            systemctl("reset-failed", UNIT)
        systemctl("start", UNIT)
        wait_until(self.ready, "HTTP readiness", 5)

    def cgroup_root(self):
        relative = property_value("ControlGroup")
        if not relative.startswith("/") or relative == "/" or ".." in Path(relative).parts:
            raise ValueError("service has no dedicated cgroup")
        return Path("/sys/fs/cgroup") / relative.lstrip("/")

    def assert_clean(self, execution_id=None):
        if self.config["AGENTBOX_MODE"] == "warm":
            status, pool = self.http("/v1/status")
            if status != 200 or pool.get("mode") != "warm" or pool["total"] > pool["max_sandboxes"]:
                raise ValueError("invalid warm pool accounting")
            if execution_id:
                for path in (STATE / "bundles" / execution_id, self.cgroup_root() / "sandboxes" / execution_id):
                    if path.exists():
                        raise ValueError("completed execution still owns resources")
            return
        bundles = STATE / "bundles"
        if bundles.exists() and any(bundles.iterdir()):
            raise ValueError("execution records remain after normal cleanup")
        root = self.cgroup_root()
        if any((root / "sandboxes").glob("ab-*")):
            raise ValueError("sandbox cgroups remain after cleanup")

    def wait_for_idle(self, description):
        if self.config["AGENTBOX_MODE"] != "warm":
            return
        def idle_available():
            status, pool = self.http("/v1/status")
            if status != 200 or not pool.get("ready"):
                raise ValueError("warm pool became unavailable before "+description)
            return pool["idle"] > 0
        wait_until(idle_available, "idle sandbox for "+description, 15)

    def wait_for_running_task(self, root, pending):
        selected = None
        def task_started():
            nonlocal selected
            selected = self.running_group(root)
            if selected is not None:
                return True
            if pending.done():
                status, result = pending.result()
                raise ValueError(f"lifecycle task ended before observation: HTTP {status}, status={result.get('status')}")
            return False
        wait_until(task_started, "running sandbox")
        return selected

    def execute(self, name, code, expected, timeout=5000):
        # Functional checks need inventory. Saturation probes call http directly
        # and still require a bounded 503, without replaying any execution.
        self.wait_for_idle(name)
        status, result = self.http("/v1/execute", {"runtime": "python", "code": code, "timeout_ms": timeout})
        if status != 200 or result.get("status") != expected or not result.get("cleanup_complete"):
            raise ValueError(f"{name} failed: HTTP {status}, status={result.get('status')}, stderr={result.get('stderr', '')[-2000:]}")
        self.assert_clean(result["execution_id"])
        self.results.append({"name": name, "execution_id": result["execution_id"], "status": result["status"], "cleanup_complete": True})
        return result

    def basic(self):
        if property_value("ActiveState") != "active" or property_value("Type") != "notify":
            raise ValueError("start the installed Type=notify service before running smoke checks")
        wait_until(self.ready, "HTTP readiness", 5)
        if property_value("Delegate") != "yes":
            raise ValueError("service is not delegated")
        if property_value("KillMode") != "mixed":
            raise ValueError("service must use mixed termination so daemon cleanup is not interrupted")
        pid = property_value("MainPID")
        membership = Path(f"/proc/{pid}/cgroup").read_text()
        if not membership.strip().endswith("/agentbox.service/manager"):
            raise ValueError("daemon is not in its manager leaf")
        group = self.cgroup_root()
        status, report = self.http("/v1/status")
        memory = report.get("memory", {})
        if status != 200 or not all(isinstance(memory.get(k), int) and memory[k] > 0 for k in ("parent_bytes", "sandbox_bytes", "reserve_bytes", "capacity_bytes")):
            raise ValueError("resolved memory budget missing")
        configured_parent = int(self.config["AGENTBOX_PARENT_MEMORY_MIB"]) << 20
        expected_parent = configured_parent or min(4 << 30, memory["capacity_bytes"]-memory["reserve_bytes"])
        if (memory["parent_bytes"] != expected_parent
                or memory["sandbox_bytes"] != int(self.config["AGENTBOX_SANDBOX_MEMORY_MIB"]) << 20
                or memory["reserve_bytes"] != int(self.config["AGENTBOX_MEMORY_RESERVE_MIB"]) << 20
                or memory["parent_bytes"]+memory["reserve_bytes"] > memory["capacity_bytes"]):
            raise ValueError("resolved memory budget differs from deployment")
        for name, expected in {"memory.max": str(expected_parent), "memory.high": str(expected_parent*3//4), "memory.swap.max": "0"}.items():
            if (group / "sandboxes" / name).read_text().strip() != expected:
                raise ValueError("unexpected parent cgroup setting: "+name)
        cpu = (group/"sandboxes/cpu.max").read_text().split()
        if self.config["AGENTBOX_QOS"] == "enforce":
            if len(cpu) != 2 or cpu[1] != "20000" or not 10000 <= int(cpu[0]) <= 80000:
                raise ValueError("controlled CPU budget is outside configured service bounds")
        elif cpu != ["80000", "20000"]:
            raise ValueError("unexpected static parent CPU setting")
        status, _ = self.http("/v1/execute", {"runtime": "python", "code": "print(1)"}, authenticate=False)
        if status != 401:
            raise ValueError("unauthenticated request was accepted")
        result = self.execute("python-main-and-permissions", "import os, __main__, pickle\nassert os.getuid()==1000\nassert globals() is __main__.__dict__\nassert 'AGENTBOX_API_TOKEN' not in os.environ\ndef f(): return 1\nassert pickle.loads(pickle.dumps(f)) is f\nprint('host-ok')", "succeeded")
        if result["stdout"] != "host-ok\n":
            raise ValueError("unexpected Python output")
        self.execute("user-failure", "raise ValueError('expected')", "failed")
        for exit_code in (125, 128):
            result = self.execute(f"user-exit-{exit_code}", f"import os; os._exit({exit_code})", "failed")
            if result.get("exit_code") != exit_code:
                raise ValueError("user exit status was not preserved")
        self.execute("timeout", "while True: pass", "timeout", 200)
        self.execute("output-limit", "import os\nwhile True: os.write(1,b'x'*8192)", "output_limit")
        self.execute("background-child", "import os,time\nif os.fork()==0:\n    time.sleep(20)\n    os._exit(0)\nprint('parent-done')", "succeeded")
        self.execute("filesystem", "import os\nassert not os.path.exists('/dev/kvm')\nopen('/workspace/marker','w').write('private')\ntry:\n    open('/etc/agentbox-test','w').write('bad')\n    raise AssertionError('rootfs writable')\nexcept OSError: pass", "succeeded")
        self.execute("fresh-workspace", "import os\nassert not os.path.exists('/workspace/marker')", "succeeded")

    def running_group(self, root):
        for group in (root / "sandboxes").glob("ab-*"):
            if self.config["AGENTBOX_MODE"] == "warm" and not (STATE / "bundles" / group.name / "task.pid").exists():
                continue
            if populated(group):
                return group
        return None

    def lifecycle(self):
        original_owner = (STATE / "owner.json").read_bytes()
        for index in range(3):
            root = self.cgroup_root()
            identities = process_snapshot(root)
            systemctl("stop", UNIT)
            self.assert_stopped(root, identities)
            self.assert_normal_stop()
            self.start()
            self.execute(f"restart-{index+1}", "print('restarted')", "succeeded")

        for crash in (False, True):
            root = self.cgroup_root()
            self.wait_for_idle("lifecycle task")
            with ThreadPoolExecutor(max_workers=1) as workers:
                pending = workers.submit(self.http, "/v1/execute", {"runtime": "python", "code": "import time; time.sleep(20)", "timeout_ms": 30000})
                group = self.wait_for_running_task(root, pending)
                for name, expected in {"cpu.max": "20000 20000", "memory.max": str(int(self.config["AGENTBOX_SANDBOX_MEMORY_MIB"]) << 20), "memory.swap.max": "0", "pids.max": "256"}.items():
                    if (group / name).read_text().strip() != expected:
                        raise ValueError("unexpected task cgroup setting: "+name)
                self.sandbox_limits = {name: (group / name).read_text().strip() for name in ("cpu.max", "memory.high", "memory.max", "memory.swap.max", "pids.max")}
                time.sleep(0.2)
                roles = set()
                for pid in (group / "cgroup.procs").read_text().split():
                    try:
                        arguments = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\x00")
                        if b"boot" in arguments:
                            roles.add("sentry")
                        if b"gofer" in arguments:
                            roles.add("gofer")
                    except FileNotFoundError:
                        pass
                if roles != {"sentry", "gofer"}:
                    raise ValueError("Sentry and Gofer are not both accounted to the task cgroup")
                self.runtime_components = sorted(roles)
                identities = process_snapshot(root)
                if int(self.config["AGENTBOX_CONCURRENCY"]) == 1:
                    code, _ = self.http("/v1/execute", {"runtime": "python", "code": "print('must-not-run')"})
                    if code != 503:
                        raise ValueError("full executor did not reject admission")
                if crash:
                    systemctl("kill", "--kill-who=main", "--signal=SIGKILL", UNIT)
                    wait_until(lambda: property_value("ActiveState") in ("failed", "inactive"), "daemon failure")
                else:
                    systemctl("stop", UNIT)
                try:
                    pending.result(timeout=10)
                except (OSError, URLError):
                    pass # A stopped or killed HTTP server may close the connection.
                self.assert_stopped(root, identities)
            if not crash:
                self.assert_normal_stop()
            self.start()
            self.execute("crash-recovery" if crash else "busy-stop-recovery", "print('recovered')", "succeeded")
            if (STATE / "owner.json").read_bytes() != original_owner:
                raise ValueError("recovery rewrote state ownership")
        self.lifecycle_checked = True

    def assert_stopped(self, root, identities):
        wait_until(lambda: not populated(root), "empty service cgroup")
        wait_until(lambda: all(process_identity(pid) != identity for pid, identity in identities.items()), "service process reaping")

    def assert_normal_stop(self):
        if property_value("Result") != "success":
            raise ValueError("normal service stop did not complete successfully")
        bundles = STATE / "bundles"
        if bundles.exists() and any(bundles.iterdir()):
            raise ValueError("normal service stop left unreconciled execution records")

    def report(self):
        root = self.cgroup_root()
        version = subprocess.run(["/opt/agentbox/.local/gvisor/runsc", "--version"], check=True, text=True, capture_output=True).stdout.strip()
        return {"recorded_at": datetime.now(timezone.utc).isoformat(), "kernel": os.uname().release,
                "systemd_version": systemctl("--version").splitlines()[0], "runsc_version": version,
                "deployment": {key: self.manifest[key] for key in ("format", "platform", "binary_sha256", "runsc_sha256", "rootfs_images")},
                "platform": self.config["AGENTBOX_PLATFORM"], "execution_mode": self.config.get("AGENTBOX_MODE", "cold"), "service": UNIT,
                "cgroup": str(root), "checks": self.results, "ready": self.ready(),
                "parent_limits": {name: (root / "sandboxes" / name).read_text().strip() for name in ("cpu.max", "memory.high", "memory.max", "memory.swap.max", "pids.max")},
                "sandbox_limits": self.sandbox_limits, "runtime_components": self.runtime_components,
                "lifecycle_checked": self.lifecycle_checked}


def main():
    parser = argparse.ArgumentParser(prog="python3 scripts/deploy.py smoke", description=__doc__)
    parser.add_argument("--lifecycle", action="store_true", help="explicitly authorize service stop/start and main-process SIGKILL checks")
    parser.add_argument("--output", help="write a new JSON evidence file, excluding tokens")
    args = parser.parse_args()
    try:
        smoke = Smoke()
        smoke.basic()
        if args.lifecycle:
            smoke.lifecycle()
        report = json.dumps(smoke.report(), indent=2)
        if args.output:
            with open(args.output, "x", encoding="utf-8") as stream:
                stream.write(report+"\n")
        print(report)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Host validation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
