"""Real systemd lifecycle checks, confined to a disposable PID-1 container."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

sys.path.insert(0, "/workspace/scripts/lib")
from smoke_host import Smoke, wait_until


def run(*arguments, success=True):
    result = subprocess.run(arguments, text=True, capture_output=True)
    if success and result.returncode:
        raise RuntimeError(f"command failed: {arguments}: {result.stderr}")
    if not success and result.returncode == 0:
        raise RuntimeError("invalid service configuration unexpectedly started")
    return result


def deploy(*arguments):
    return run("python3", "/workspace/scripts/deploy.py", "base", "--source", "/workspace", *arguments)


def check_parallel_inventory_shutdown(smoke, token_path, configuration):
    """Eight real idle sandboxes, each delete delayed by two seconds."""
    run("systemctl", "stop", "agentbox.service")
    smoke.assert_normal_stop()
    wrapper = Path("/opt/agentbox/delayed-runsc")
    dropin = Path("/etc/systemd/system/agentbox.service.d/drain-test.conf")
    unit = Path("/etc/systemd/system/agentbox.service").read_text()
    command = next(line for line in unit.splitlines() if line.startswith("ExecStart="))
    wrapper.write_text('#!/bin/sh\nfor arg do\n  if [ "$arg" = delete ]; then sleep 2; fi\ndone\nexec /opt/agentbox/.local/gvisor/runsc "$@"\n')
    wrapper.chmod(0o755)
    dropin.parent.mkdir(exist_ok=True)
    dropin.write_text("[Service]\nExecStart=\n"+command.replace("/opt/agentbox/.local/gvisor/runsc", str(wrapper))+"\n")
    token_path.write_bytes(configuration+b"AGENTBOX_WARM_SIZE=8\nAGENTBOX_MAX_SANDBOXES=8\nAGENTBOX_CREATE_CONCURRENCY=2\n")
    run("systemctl", "daemon-reload")
    try:
        smoke.start()
        wait_until(lambda: smoke.http("/v1/status")[1].get("idle") == 8, "eight idle sandboxes", 20)
        began = time.monotonic()
        run("systemctl", "stop", "agentbox.service")
        elapsed = time.monotonic()-began
        smoke.assert_normal_stop()
        if elapsed >= 12:
            raise RuntimeError(f"eight-instance shutdown did not run in parallel: {elapsed:.2f}s")
        return {"idle_instances": 8, "injected_delete_delay_seconds": 2, "elapsed_seconds": elapsed}
    finally:
        run("systemctl", "stop", "agentbox.service")
        token_path.write_bytes(configuration)
        dropin.unlink(missing_ok=True)
        wrapper.unlink(missing_ok=True)
        run("systemctl", "daemon-reload")


def check_observation_configuration(smoke, token_path, configuration):
    """Exercise unit flag expansion and a missing protected service."""
    run("systemctl", "stop", "agentbox.service")
    smoke.assert_normal_stop()
    token_path.write_bytes(configuration+b"AGENTBOX_QOS=observe\nAGENTBOX_QOS_INTERVAL=100ms\n"
                           b"AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/agentbox-missing-core\n")
    try:
        smoke.start()
        def degraded():
            code, status = smoke.http("/v1/status")
            report = status.get("qos", {})
            return (code == 200 and status["ready"] and report.get("mode") == "observe"
                    and report["state"] == "degraded" and not report["stale"]
                    and any(s["name"] == "protected" and not s["available"] and s["sample"] is None
                            for s in report["sources"]))
        wait_until(degraded, "observe mode with unavailable protected service", 20)
        smoke.execute("observe-missing-core", "print('observation failure does not stop execution')", "succeeded")
        return ["unit-observe-flags", "missing-source-keeps-ready", "observe-execution"]
    finally:
        run("systemctl", "stop", "agentbox.service")
        smoke.assert_normal_stop()
        token_path.write_bytes(configuration)


def main():
    if not Path("/.dockerenv").exists() or Path("/proc/1/comm").read_text().strip() != "systemd":
        raise RuntimeError("this test must run inside its disposable systemd container")
    # Docker exec begins outside systemd's own units. Move the test controller
    # into init.scope before it enables controllers in the delegated tree.
    Path("/sys/fs/cgroup/init.scope/cgroup.procs").write_text(str(os.getpid()))
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    result = deploy("--platform", os.environ.get("AGENTBOX_PLATFORM", "kvm"))
    print(result.stdout.strip())
    manifest = json.loads(Path("/opt/agentbox/INSTALL.json").read_text())
    installed_hash = hashlib.sha256(Path("/opt/agentbox/bin/agentboxd").read_bytes()).hexdigest()
    if installed_hash != manifest["binary_sha256"]:
        raise RuntimeError("installed binary does not match deployment evidence")
    for path in (Path("/opt/agentbox/bin/agentboxd"), Path("/opt/agentbox/.local/gvisor/runsc"), Path("/etc/agentbox.env")):
        if path.stat().st_uid != 0:
            raise RuntimeError("deployment is not root-owned")
    token_path = Path("/etc/agentbox.env")
    mode = os.environ.get("AGENTBOX_TEST_MODE", "cold")
    if mode not in ("cold", "warm"):
        raise ValueError("invalid test mode")
    token_path.write_text(token_path.read_text()+f"AGENTBOX_MODE={mode}\n")
    configuration = token_path.read_bytes()
    # Type=notify must report initialization failure, not an active service.
    try:
        token_path.write_text("AGENTBOX_API_TOKEN=invalid\n")
        run("systemctl", "start", "agentbox.service", success=False)
    finally:
        token_path.write_bytes(configuration)
    smoke = Smoke()
    smoke.start()
    smoke.basic()
    smoke.lifecycle()
    report = smoke.report()
    if mode == "warm":
        report["parallel_inventory_shutdown"] = check_parallel_inventory_shutdown(smoke, token_path, configuration)
    report["observation_checks"] = check_observation_configuration(smoke, token_path, configuration)
    run("systemctl", "stop", "agentbox.service")
    smoke.assert_normal_stop()
    owner = Path("/var/lib/agentbox/owner.json").read_bytes()
    deploy("--update")
    if token_path.read_bytes() != configuration or Path("/var/lib/agentbox/owner.json").read_bytes() != owner:
        raise RuntimeError("update changed configuration or state ownership")
    smoke.start()
    smoke.execute("after-update", "print('updated')", "succeeded")
    run("systemctl", "stop", "agentbox.service")
    smoke.assert_normal_stop()
    deploy("--rollback")
    smoke.start()
    smoke.execute("after-rollback", "print('rolled back')", "succeeded")
    run("python3", "/workspace/scripts/deploy.py", "base", "--uninstall")
    if not token_path.exists() or Path("/var/lib/agentbox/owner.json").read_bytes() != owner:
        raise RuntimeError("uninstall removed retained data")
    deploy()
    smoke.start()
    smoke.execute("after-reinstall", "print('reinstalled')", "succeeded")
    run("python3", "/workspace/scripts/deploy.py", "base", "--uninstall", "--purge")
    if token_path.exists() or Path("/var/lib/agentbox").exists():
        raise RuntimeError("explicit purge did not remove deployment data")
    report["deployment_checks"] = ["invalid-config-rejected", "update", "rollback", "uninstall-retains-data", "reinstall", "explicit-purge"]
    report["ready"] = False
    report["final_service_state"] = "uninstalled"
    report["systemd_version"] = run("systemctl", "--version").stdout.splitlines()[0]
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
