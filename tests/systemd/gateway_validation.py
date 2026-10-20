"""Gateway lifecycle acceptance, exclusively in the disposable systemd container."""

import grp
import json
import os
from pathlib import Path
import pwd
import stat
import subprocess
import sys
from urllib.error import HTTPError, URLError
from urllib.request import Request, ProxyHandler, build_opener

sys.path.insert(0, "/workspace/scripts")
from smoke_host import Smoke, wait_until
from systemd_common import load_gateway_config


def run(*args, success=True):
    result = subprocess.run(args, capture_output=True, text=True, timeout=120)
    if success != (result.returncode == 0):
        raise RuntimeError(f"unexpected command outcome: {args}: {result.stderr}")
    return result


def deploy(gateway=False, *args):
    script = "install-gateway-systemd.sh" if gateway else "install-systemd.sh"
    return run("bash", "/workspace/scripts/"+script, "--source", "/workspace", *args)


def main():
    if not Path("/.dockerenv").exists() or Path("/proc/1/comm").read_text().strip() != "systemd":
        raise RuntimeError("requires disposable systemd PID-1 container")
    Path("/sys/fs/cgroup/init.scope/cgroup.procs").write_text(str(os.getpid()))
    deploy(False, "--platform", os.environ.get("AGENTBOX_PLATFORM", "systrap"))
    base = Path("/etc/agentbox.env").read_bytes()
    deploy(True)
    configuration = Path("/etc/agentbox-gateway.env").read_bytes()
    settings = load_gateway_config(Path("/etc/agentbox-gateway.env"))
    client = build_opener(ProxyHandler({}))

    def http(path, body=None, token=None):
        data = json.dumps(body).encode() if body is not None else None
        request = Request("http://"+settings["AGENTBOX_GATEWAY_LISTEN"]+path, data=data)
        request.add_header("Authorization", "Bearer "+(settings["AGENTBOX_GATEWAY_TOKEN"] if token is None else token))
        request.add_header("Content-Type", "application/json")
        try:
            response = client.open(request, timeout=50)
        except HTTPError as error:
            response = error
        with response:
            return response.code, json.loads(response.read(8 << 20))

    def ready():
        try:
            return http("/readyz")[0] == 200
        except (OSError, URLError):
            return False

    def start():
        run("systemctl", "start", "agentbox-gateway.service")
        wait_until(ready, "paired Gateway readiness", 10)

    executions = []
    def execute(name):
        code, result = http("/v1/execute", {"runtime": "python", "code": "import os\nassert not any(k in os.environ for k in ('AGENTBOX_API_TOKEN','AGENTBOX_DAEMON_TOKEN','AGENTBOX_GATEWAY_TOKEN'))\nprint('paired-ok')", "request_id": name})
        if code != 200 or result.get("status") != "succeeded" or result.get("stdout") != "paired-ok\n" or not result.get("cleanup_complete"):
            raise RuntimeError(f"paired execution failed: {name}: HTTP {code}, status={result.get('status')}")
        Smoke().assert_clean(result["execution_id"])
        executions.append(name)

    start()
    user, group = pwd.getpwnam("agentbox-gateway"), grp.getgrnam("agentbox-gateway")
    pid = run("systemctl", "show", "agentbox-gateway.service", "-p", "MainPID", "--value").stdout.strip()
    if user.pw_uid == 0 or f"Uid:\t{user.pw_uid}\t{user.pw_uid}\t{user.pw_uid}\t{user.pw_uid}" not in Path(f"/proc/{pid}/status").read_text():
        raise RuntimeError("Gateway service is not running as its ordinary account")
    endpoint = Path("/run/agentbox/daemon.sock")
    for path, mode in ((endpoint.parent, 0o750), (endpoint, 0o660)):
        info = path.stat()
        if info.st_uid != 0 or info.st_gid != group.gr_gid or stat.S_IMODE(info.st_mode) != mode:
            raise RuntimeError("incorrect paired Unix socket ownership or mode")
    for path in ("/etc/agentbox.env", "/etc/agentbox-gateway.env", "/etc/agentbox-gateway-daemon.env"):
        run("runuser", "-u", "agentbox-gateway", "--", "test", "-r", path, success=False)
    # Use a different unprivileged identity to check the kernel permission boundary.
    denial = "import socket\ns=socket.socket(socket.AF_UNIX)\ntry:s.connect('/run/agentbox/daemon.sock')\nexcept PermissionError:pass\nelse:raise RuntimeError('unrelated user connected')"
    run("runuser", "-u", "nobody", "--", "python3", "-c", denial)
    if http("/v1/status", token=settings["AGENTBOX_DAEMON_TOKEN"])[0] != 401:
        raise RuntimeError("internal token accepted at external boundary")
    execute("paired-initial")
    owner = Path("/var/lib/agentbox/owner.json").read_bytes()
    before = endpoint.stat().st_ino
    run("systemctl", "restart", "agentbox.service")
    gateway_after_restart = run("systemctl", "show", "agentbox-gateway.service", "-p", "ActiveState", "--value").stdout.strip()
    # This must not silently leave the ordinary entry point stopped after backend restart.
    wait_until(ready, "Gateway recovery after daemon restart", 5)
    execute("after-daemon-restart")
    run("systemctl", "stop", "agentbox.service")
    if run("systemctl", "show", "agentbox-gateway.service", "-p", "ActiveState", "--value").stdout.strip() != "inactive":
        raise RuntimeError("stopping daemon did not stop dependent Gateway")
    Smoke().assert_normal_stop()
    start()
    execute("after-pair-restart")
    run("systemctl", "stop", "agentbox-gateway.service", "agentbox.service")
    Smoke().assert_normal_stop()
    deploy(True, "--update")
    if configuration != Path("/etc/agentbox-gateway.env").read_bytes():
        raise RuntimeError("Gateway update changed credentials")
    start()
    execute("after-gateway-update")
    run("systemctl", "stop", "agentbox-gateway.service", "agentbox.service")
    Smoke().assert_normal_stop()
    deploy(True, "--uninstall")
    if Path("/etc/agentbox-gateway-daemon.env").exists() or Path("/etc/systemd/system/agentbox.service.d/gateway.conf").exists():
        raise RuntimeError("Gateway backend override survived removal")
    standalone = Smoke()
    standalone.start()
    standalone.execute("standalone-restored", "print('standalone')", "succeeded")
    run("systemctl", "stop", "agentbox.service")
    standalone.assert_normal_stop()
    if base != Path("/etc/agentbox.env").read_bytes() or owner != Path("/var/lib/agentbox/owner.json").read_bytes():
        raise RuntimeError("paired lifecycle changed base configuration or owner")
    deploy(True)
    if configuration != Path("/etc/agentbox-gateway.env").read_bytes():
        raise RuntimeError("re-pair changed retained credentials")
    start()
    execute("after-repair")
    run("systemctl", "stop", "agentbox-gateway.service", "agentbox.service")
    Smoke().assert_normal_stop()
    deploy(True, "--uninstall", "--purge")
    deploy(False, "--uninstall", "--purge")
    if Path("/etc/agentbox-gateway.env").exists() or Path("/var/lib/agentbox").exists():
        raise RuntimeError("explicit purge left managed configuration or task state")
    print(json.dumps({"gateway_systemd": "passed", "gateway_uid": user.pw_uid,
        "socket_mode": "0660", "directory_mode": "0750", "unrelated_user_denied": True,
        "credential_files_denied": True, "gateway_state_after_daemon_restart": gateway_after_restart,
        "socket_inode_before_restart": before, "executions": executions,
        "checks": ["dependency-start", "daemon-restart", "dependency-stop", "pair-restart", "gateway-update", "remove-restores-standalone", "repair-retains-credentials", "purge"],
        "systemd_version": run("systemctl", "--version").stdout.splitlines()[0]}, indent=2))


if __name__ == "__main__":
    main()
