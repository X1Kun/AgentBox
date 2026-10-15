"""Real systemd lifecycle checks, confined to a disposable PID-1 container."""

import json
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, "/workspace/scripts")
from smoke_host import Smoke


def run(*arguments, success=True):
    result = subprocess.run(arguments, text=True, capture_output=True)
    if success and result.returncode:
        raise RuntimeError(f"command failed: {arguments}: {result.stderr}")
    if not success and result.returncode == 0:
        raise RuntimeError("invalid service configuration unexpectedly started")
    return result


def deploy(*arguments):
    return run("bash", "/workspace/scripts/install-systemd.sh", "--source", "/workspace", *arguments)


def main():
    if not Path("/.dockerenv").exists() or Path("/proc/1/comm").read_text().strip() != "systemd":
        raise RuntimeError("this test must run inside its disposable systemd container")
    # Docker exec begins outside systemd's own units. Move the test controller
    # into init.scope before it enables controllers in the delegated tree.
    Path("/sys/fs/cgroup/init.scope/cgroup.procs").write_text(str(os.getpid()))
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    result = deploy("--platform", os.environ.get("AGENTBOX_PLATFORM", "kvm"))
    print(result.stdout.strip())
    for path in (Path("/opt/agentbox/bin/agentboxd"), Path("/opt/agentbox/.local/gvisor/runsc"), Path("/etc/agentbox.env")):
        if path.stat().st_uid != 0:
            raise RuntimeError("deployment is not root-owned")
    token_path = Path("/etc/agentbox.env")
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
    run("systemctl", "stop", "agentbox.service")
    owner = Path("/var/lib/agentbox/owner.json").read_bytes()
    deploy("--update")
    if token_path.read_bytes() != configuration or Path("/var/lib/agentbox/owner.json").read_bytes() != owner:
        raise RuntimeError("update changed configuration or state ownership")
    smoke.start()
    smoke.execute("after-update", "print('updated')", "succeeded")
    run("systemctl", "stop", "agentbox.service")
    deploy("--rollback")
    smoke.start()
    smoke.execute("after-rollback", "print('rolled back')", "succeeded")
    run("bash", "/workspace/scripts/uninstall-systemd.sh")
    if not token_path.exists() or Path("/var/lib/agentbox/owner.json").read_bytes() != owner:
        raise RuntimeError("uninstall removed retained data")
    deploy()
    smoke.start()
    smoke.execute("after-reinstall", "print('reinstalled')", "succeeded")
    run("bash", "/workspace/scripts/uninstall-systemd.sh", "--purge")
    if token_path.exists() or Path("/var/lib/agentbox").exists():
        raise RuntimeError("explicit purge did not remove deployment data")
    report["deployment_checks"] = ["invalid-config-rejected", "update", "rollback", "uninstall-retains-data", "reinstall", "explicit-purge"]
    report["ready"] = False
    report["final_service_state"] = "uninstalled"
    report["systemd_version"] = run("systemctl", "--version").stdout.splitlines()[0]
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
