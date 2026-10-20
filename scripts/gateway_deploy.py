#!/usr/bin/env python3
"""Install the optional Gateway on an existing managed, stopped AgentBox service."""

import argparse
import grp
import json
import os
from pathlib import Path
import pwd
import secrets
import subprocess
import sys

from systemd_common import load_config, load_gateway_config
from systemd_deploy import Deployment, atomic_bytes, atomic_copy, ensure_directory, run, sha256

UNIT = "agentbox-gateway.service"
IDENTITY = "agentbox-gateway"


class GatewayDeployment:
    def __init__(self, args):
        self.args = args
        self.base = Deployment(args)
        self.binary = self.base.app/"bin/agentbox-gateway"
        self.manifest = self.base.app/"GATEWAY_INSTALL.json"
        self.unit = self.base.root/"etc/systemd/system"/UNIT
        self.dropin = self.base.root/"etc/systemd/system/agentbox.service.d/gateway.conf"
        self.config = self.base.root/"etc/agentbox-gateway.env"
        self.backend_config = self.base.root/"etc/agentbox-gateway-daemon.env"
        self.managed = (self.binary, self.unit, self.dropin, self.backend_config, self.manifest)

    def validate(self):
        self.base.validate_paths()
        self.base.existing()
        for path in (*self.managed, self.config):
            for ancestor in (path, *path.parents):
                if ancestor == self.base.root:
                    break
                if ancestor.is_symlink():
                    raise ValueError(f"Gateway administrative path must not be a symlink: {ancestor}")
                if self.base.live and ancestor.exists() and (ancestor.stat().st_uid != 0 or ancestor.stat().st_mode & 0o022):
                    raise ValueError("Gateway administrative paths must be root-owned and not writable by group/others")
            if path.exists():
                if not path.is_file():
                    raise ValueError("Gateway administrative path must be a regular file")
                if self.base.live and path.stat().st_uid != 0:
                    raise ValueError("Gateway deployment must be root-owned")
                if path in (self.config, self.backend_config) and path.stat().st_mode & 0o077:
                    raise ValueError("Gateway credential files must be private (0600)")

    def require_stopped(self):
        self.base.require_stopped()
        if self.base.live:
            state = run("systemctl", "show", UNIT, "--property=ActiveState", "--value")
            if state not in ("inactive", "failed", ""):
                raise ValueError("stop agentbox-gateway.service before changing its deployment")
        self.base.require_reconciled()

    def ensure_identity(self):
        if not self.base.live:
            return
        try:
            group = grp.getgrnam(IDENTITY)
        except KeyError:
            run("groupadd", "--system", IDENTITY)
            group = grp.getgrnam(IDENTITY)
        try:
            user = pwd.getpwnam(IDENTITY)
        except KeyError:
            run("useradd", "--system", "--gid", IDENTITY, "--home-dir", "/nonexistent",
                "--no-create-home", "--shell", "/usr/sbin/nologin", IDENTITY)
            user = pwd.getpwnam(IDENTITY)
        if user.pw_uid == 0 or group.gr_gid == 0 or user.pw_gid != group.gr_gid:
            raise ValueError("Gateway account must be non-root with its dedicated primary group")

    def install(self):
        self.require_stopped()
        previous = None
        if self.manifest.exists():
            previous = json.loads(self.manifest.read_text())
            if not self.args.update or previous.get("format") != 1:
                raise ValueError("Gateway already exists; use --update for a managed installation")
        elif self.args.update or any(path.exists() for path in self.managed):
            raise ValueError("Gateway installation is missing or contains unmanaged files")
        installed = self.base.existing()
        daemon = self.base.app/"bin/agentboxd"
        if sha256(daemon) != installed["binary_sha256"]:
            raise ValueError("installed daemon does not match its manifest")
        probe = subprocess.run([str(daemon), "-help"], timeout=3, capture_output=True, text=True,
                               env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8"})
        if probe.returncode or "unix:" not in probe.stdout+probe.stderr:
            raise ValueError("update the installed daemon to a version supporting Unix HTTP first")
        source_binary = self.base.source/"bin/agentbox-gateway"
        source_unit = self.base.source/"deploy/systemd"/UNIT
        source_dropin = self.base.source/"deploy/systemd/agentbox-gateway-daemon.conf"
        if not source_binary.is_file() or not os.access(source_binary, os.X_OK) or not source_unit.is_file() or not source_dropin.is_file():
            raise ValueError("build Gateway and prepare its unit/drop-in before installing")
        if self.config.exists():
            settings = load_gateway_config(self.config)
            if self.args.listen and self.args.listen != settings["AGENTBOX_GATEWAY_LISTEN"]:
                raise ValueError("--listen conflicts with retained Gateway configuration")
            config_data = self.config.read_bytes()
        else:
            base = load_config(self.base.config)
            settings = {"AGENTBOX_GATEWAY_TOKEN": base["AGENTBOX_API_TOKEN"],
                        "AGENTBOX_DAEMON_TOKEN": secrets.token_hex(32),
                        "AGENTBOX_GATEWAY_LISTEN": self.args.listen or base["AGENTBOX_LISTEN"],
                        "AGENTBOX_GATEWAY_MAX_INFLIGHT": "8", "AGENTBOX_GATEWAY_TIMEOUT": "50s"}
            config_data = "".join(key+"="+value+"\n" for key,value in settings.items()).encode()
            # Validate before any deployment mutation; this never logs tokens.
            import tempfile
            with tempfile.TemporaryDirectory() as directory:
                candidate = Path(directory)/"config"
                candidate.write_bytes(config_data)
                load_gateway_config(candidate)
        backend = ("AGENTBOX_LISTEN=unix:/run/agentbox/daemon.sock\nAGENTBOX_API_TOKEN="+
                   settings["AGENTBOX_DAEMON_TOKEN"]+"\n").encode()
        self.ensure_identity()
        snapshots = {path: (path.read_bytes(), path.stat().st_mode & 0o777) if path.exists() else None
                     for path in (*self.managed, self.config)}
        try:
            ensure_directory(self.dropin.parent, 0o755)
            atomic_copy(source_binary, self.binary, 0o755)
            atomic_bytes(config_data, self.config, 0o600)
            atomic_bytes(backend, self.backend_config, 0o600)
            atomic_copy(source_unit, self.unit, 0o644)
            atomic_copy(source_dropin, self.dropin, 0o644)
            manifest = {"format": 1, "binary_sha256": sha256(source_binary),
                        "daemon_binary_sha256": installed["binary_sha256"]}
            atomic_bytes(json.dumps(manifest, indent=2).encode()+b"\n", self.manifest, 0o644)
            self.base.verify(self.unit)
            self.base.reload()
        except Exception:
            for path, snapshot in snapshots.items():
                if snapshot is None:
                    path.unlink(missing_ok=True)
                else:
                    atomic_bytes(snapshot[0], path, snapshot[1])
            self.base.reload()
            raise
        print("Gateway installed. Services are stopped and not enabled; credentials were preserved or generated privately.")

    def uninstall(self):
        self.require_stopped()
        if not self.manifest.is_file() or json.loads(self.manifest.read_text()).get("format") != 1:
            raise ValueError("Gateway is not a managed installation")
        if self.base.live:
            run("systemctl", "disable", UNIT)
        for path in self.managed:
            path.unlink(missing_ok=True)
        if self.args.purge:
            self.config.unlink(missing_ok=True)
        self.base.reload()
        print("Gateway removed; standalone daemon configuration and task state preserved. Gateway account retained.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_mutually_exclusive_group()
    actions.add_argument("--update", action="store_true")
    actions.add_argument("--uninstall", action="store_true")
    parser.add_argument("--purge", action="store_true")
    parser.add_argument("--source", default=str(Path(__file__).resolve().parent.parent))
    parser.add_argument("--destdir", default="/")
    parser.add_argument("--listen")
    args = parser.parse_args()
    if args.purge and not args.uninstall:
        parser.error("--purge is only valid with --uninstall")
    deployment = GatewayDeployment(args)
    try:
        deployment.validate()
        with deployment.base.deployment_lock(), deployment.base.state_lock():
            if args.uninstall:
                deployment.uninstall()
            else:
                deployment.install()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"Gateway deployment failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
