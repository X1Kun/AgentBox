#!/usr/bin/env python3
"""Manage a fixed AgentBox deployment; --destdir stages files without systemd."""

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile

from systemd_common import load_config

UNIT = "agentbox.service"
FORMAT = 1


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def run(*args):
    return subprocess.run(args, check=True, text=True, capture_output=True).stdout.strip()


def atomic_copy(source, target, mode):
    fd, name = tempfile.mkstemp(prefix=".agentbox-", dir=target.parent)
    os.close(fd)
    temporary = Path(name)
    try:
        shutil.copyfile(source, temporary)
        temporary.chmod(mode)
        os.replace(temporary, target)
    finally:
        temporary.unlink(missing_ok=True)


def atomic_bytes(data, target, mode):
    fd, name = tempfile.mkstemp(prefix=".agentbox-", dir=target.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.chmod(mode)
        os.replace(temporary, target)
    finally:
        temporary.unlink(missing_ok=True)


def ensure_directory(path, mode):
    existed = path.exists()
    path.mkdir(parents=True, exist_ok=True)
    if not existed:
        path.chmod(mode)


class Deployment:
    def __init__(self, args):
        self.args = args
        self.requested_root = Path(args.destdir).absolute()
        self.root = Path(os.path.normpath("/"+str(self.requested_root).lstrip("/")))
        self.live = self.root == Path("/")
        self.app = self.root / "opt/agentbox"
        self.unit = self.root / "etc/systemd/system" / UNIT
        self.config = self.root / "etc/agentbox.env"
        self.state = self.root / "var/lib/agentbox"
        self.manifest = self.app / "INSTALL.json"
        self.source = Path(args.source).resolve()

    def validate_paths(self):
        if self.live and os.geteuid() != 0:
            raise ValueError("host deployment requires root; run the prepared command with sudo")
        if any(path.is_symlink() for path in (self.requested_root, *self.requested_root.parents, self.root, *self.root.parents)):
            raise ValueError("destdir and its ancestors must not be symlinks")
        # Reject symlinked administrative paths before writing or deleting.
        for path in (self.app, self.unit, self.config, self.state):
            for ancestor in (path, *path.parents):
                if ancestor == self.root:
                    break
                if ancestor.is_symlink():
                    raise ValueError(f"administrative path must not be a symlink: {ancestor}")
        if self.live:
            for path in (self.app, self.unit, self.config, self.state):
                if path.exists() and path.stat().st_uid != 0:
                    raise ValueError(f"administrative path must be root-owned: {path}")
            for path in (self.app, self.unit):
                if path.exists() and path.stat().st_mode & 0o022:
                    raise ValueError(f"administrative path is writable by group or others: {path}")
            if self.config.exists() and self.config.stat().st_mode & 0o077:
                raise ValueError("existing /etc/agentbox.env must be private (0600)")

    def existing(self):
        if not self.manifest.is_file() or self.manifest.is_symlink():
            raise ValueError("deployment is not managed by AgentBox (INSTALL.json missing)")
        manifest = json.loads(self.manifest.read_text())
        if manifest.get("format") != FORMAT:
            raise ValueError("unsupported deployment manifest")
        return manifest

    def require_stopped(self):
        if not self.live:
            return
        state = run("systemctl", "show", UNIT, "--property=ActiveState", "--value")
        if state not in ("inactive", "failed", ""):
            raise ValueError("stop agentbox.service before installing, updating, or rolling back")

    @contextlib.contextmanager
    def deployment_lock(self):
        path = Path("/run/lock/agentbox-deploy.lock") if self.live else self.root / ".agentbox-deploy.lock"
        descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        try:
            info = os.fstat(descriptor)
            if info.st_uid != os.geteuid() or info.st_mode & 0o077:
                raise ValueError("deployment lock must be private and owned by the administrator")
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ValueError("another deployment operation is running") from None
            yield
        finally:
            os.close(descriptor)

    @contextlib.contextmanager
    def state_lock(self):
        lock = self.state / "daemon.lock"
        if not lock.exists():
            yield
            return
        descriptor = os.open(lock, os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC)
        try:
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ValueError("daemon state is locked; a daemon is still running") from None
            yield
        finally:
            os.close(descriptor)

    def require_reconciled(self):
        bundles = self.state / "bundles"
        if bundles.exists() and any(bundles.iterdir()):
            raise ValueError("unreconciled execution records remain; start the existing service to recover them first")

    def validate_source(self):
        for name in ("bin/agentboxd", ".local/gvisor/runsc", ".local/rootfs/usr/local/bin/python3", "deploy/systemd/agentbox.service"):
            if not (self.source / name).is_file():
                raise ValueError(f"missing deployment input: {name}")
        for name in ("bin/agentboxd", ".local/gvisor/runsc", ".local/rootfs/usr/local/bin/python3"):
            if not os.access(self.source / name, os.X_OK):
                raise ValueError(f"deployment input is not executable: {name}")
        if not (self.source / ".local/gvisor/gvisor-bin").is_dir():
            raise ValueError("runsc sidecar directory is missing")
        images = json.loads((self.source / ".local/rootfs-image.json").read_text())
        if not isinstance(images, list) or not images or not all(isinstance(value, str) and "@sha256:" in value for value in images):
            raise ValueError("rootfs-image.json must record image digests")
        return images

    def make_manifest(self, images, platform):
        return {"format": FORMAT, "platform": platform, "binary_sha256": sha256(self.source / "bin/agentboxd"),
                "runsc_sha256": sha256(self.source / ".local/gvisor/runsc"), "rootfs_images": images}

    def reload(self):
        if self.live:
            run("systemctl", "daemon-reload")

    def verify(self, unit):
        if self.live:
            run("systemd-analyze", "verify", str(unit))

    def install(self):
        self.require_stopped()
        if self.app.exists() or self.unit.exists():
            raise ValueError("deployment already exists; use --update for a managed installation")
        images = self.validate_source()
        existing_config = load_config(self.config) if self.config.exists() else None
        platform = self.args.platform or (existing_config["AGENTBOX_PLATFORM"] if existing_config else "kvm")
        if existing_config and existing_config["AGENTBOX_PLATFORM"] != platform:
            raise ValueError("requested platform does not match the retained configuration")
        if self.state.exists() and any(self.state.iterdir()):
            self.require_reconciled()
            expected = {"cgroup_root": "/sys/fs/cgroup/system.slice/agentbox.service", "rootfs": "/opt/agentbox/.local/rootfs", "platform": platform}
            owner = self.state / "owner.json"
            if not owner.is_file() or json.loads(owner.read_text()) != expected:
                raise ValueError("retained state does not match the fixed service configuration")
        ensure_directory(self.app.parent, 0o755)
        ensure_directory(self.unit.parent, 0o755)
        with tempfile.TemporaryDirectory(prefix=".agentbox-install-", dir=self.app.parent) as directory:
            staged = Path(directory) / "app"
            ensure_directory(staged / "bin", 0o755)
            ensure_directory(staged / ".local", 0o755)
            atomic_copy(self.source / "bin/agentboxd", staged / "bin/agentboxd", 0o755)
            shutil.copytree(self.source / ".local/gvisor", staged / ".local/gvisor", symlinks=True)
            shutil.copytree(self.source / ".local/rootfs", staged / ".local/rootfs", symlinks=True)
            (staged / ".local/rootfs").chmod(0o755)
            atomic_copy(self.source / ".local/rootfs-image.json", staged / ".local/rootfs-image.json", 0o644)
            manifest = self.make_manifest(images, platform)
            atomic_bytes(json.dumps(manifest, indent=2).encode()+b"\n", staged / "INSTALL.json", 0o644)
            staged.chmod(0o755)
            os.replace(staged, self.app)
        # Preserve any pre-provisioned token. Generate it only on first install.
        config_created = not self.config.exists()
        try:
            self.verify(self.source / "deploy/systemd/agentbox.service")
            if config_created:
                body = f"AGENTBOX_API_TOKEN={secrets.token_hex(32)}\nAGENTBOX_PLATFORM={platform}\nAGENTBOX_LISTEN=127.0.0.1:8080\nAGENTBOX_CONCURRENCY=1\n"
                atomic_bytes(body.encode(), self.config, 0o600)
            atomic_copy(self.source / "deploy/systemd/agentbox.service", self.unit, 0o644)
            self.reload()
        except Exception:
            self.unit.unlink(missing_ok=True)
            if config_created:
                self.config.unlink(missing_ok=True)
            shutil.rmtree(self.app)
            raise
        print("AgentBox installed. Configuration is private; service is not started or enabled.")

    def update(self):
        previous = self.existing()
        self.require_stopped()
        self.require_reconciled()
        if load_config(self.config)["AGENTBOX_PLATFORM"] != previous["platform"]:
            raise ValueError("platform changes require an explicit state migration")
        images = self.validate_source()
        incoming = self.make_manifest(images, previous["platform"])
        if self.args.platform and self.args.platform != previous["platform"]:
            raise ValueError("platform changes require an explicit state migration")
        if any(incoming[key] != previous[key] for key in ("runsc_sha256", "rootfs_images")):
            raise ValueError("binary-only update cannot change runsc or rootfs; preserve the original dependencies")
        self.verify(self.source / "deploy/systemd/agentbox.service")
        backup = self.app / "previous"
        if backup.exists():
            shutil.rmtree(backup)
        ensure_directory(backup, 0o700)
        for source, name in ((self.app / "bin/agentboxd", "agentboxd"), (self.unit, "agentbox.service"), (self.manifest, "INSTALL.json")):
            atomic_copy(source, backup / name, 0o600)
        try:
            atomic_copy(self.source / "bin/agentboxd", self.app / "bin/agentboxd", 0o755)
            atomic_copy(self.source / "deploy/systemd/agentbox.service", self.unit, 0o644)
            atomic_bytes(json.dumps(incoming, indent=2).encode()+b"\n", self.manifest, 0o644)
            self.reload()
        except Exception:
            self.restore(backup)
            raise
        print("AgentBox binary and unit updated. Dependencies, configuration, and state were preserved.")

    def restore(self, backup):
        for name, target, mode in (("agentboxd", self.app / "bin/agentboxd", 0o755), ("agentbox.service", self.unit, 0o644), ("INSTALL.json", self.manifest, 0o644)):
            atomic_copy(backup / name, target, mode)
        self.reload()

    def rollback(self):
        current = self.existing()
        self.require_stopped()
        self.require_reconciled()
        if load_config(self.config)["AGENTBOX_PLATFORM"] != current["platform"]:
            raise ValueError("platform changes require an explicit state migration")
        backup = self.app / "previous"
        old = json.loads((backup / "INSTALL.json").read_text())
        if any(old[key] != current[key] for key in ("format", "platform", "runsc_sha256", "rootfs_images")):
            raise ValueError("previous deployment dependencies do not match")
        if sha256(backup / "agentboxd") != old["binary_sha256"]:
            raise ValueError("previous binary checksum does not match")
        self.restore(backup)
        print("Previous binary and unit restored. Configuration and state were preserved.")

    def uninstall(self):
        self.existing()
        if self.live:
            run("systemctl", "disable", UNIT)
        self.require_reconciled()
        self.unit.unlink(missing_ok=True)
        self.reload()
        shutil.rmtree(self.app)
        if self.args.purge:
            self.config.unlink(missing_ok=True)
            if self.state.exists():
                shutil.rmtree(self.state)
        print("AgentBox uninstalled. " + ("Configuration and state removed." if self.args.purge else "Configuration and state retained."))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_mutually_exclusive_group()
    actions.add_argument("--update", action="store_true")
    actions.add_argument("--rollback", action="store_true")
    actions.add_argument("--uninstall", action="store_true")
    parser.add_argument("--source", default=str(Path(__file__).resolve().parent.parent))
    parser.add_argument("--destdir", default="/", help="stage files below this directory; never manage host services")
    parser.add_argument("--platform", choices=("kvm", "systrap"))
    parser.add_argument("--purge", action="store_true", help="explicitly remove retained state and token on uninstall")
    args = parser.parse_args()
    if args.purge and not args.uninstall:
        parser.error("--purge is only valid with --uninstall")
    deployment = Deployment(args)
    try:
        deployment.validate_paths()
        # Stop before taking the state lock, allowing daemon cleanup to finish.
        with deployment.deployment_lock():
            if args.uninstall and deployment.live:
                run("systemctl", "stop", UNIT)
            with deployment.state_lock():
                if args.uninstall:
                    deployment.uninstall()
                elif args.rollback:
                    deployment.rollback()
                elif args.update:
                    deployment.update()
                else:
                    deployment.install()
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        # Do not print captured subprocess output: it may include configuration.
        print(f"Deployment failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
