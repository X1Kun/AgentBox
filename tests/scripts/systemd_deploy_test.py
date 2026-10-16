import fcntl
from concurrent.futures import Future
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch


REPOSITORY = Path(__file__).resolve().parents[2]
SCRIPT = REPOSITORY / "scripts/systemd_deploy.py"
sys.path.insert(0, str(REPOSITORY / "scripts"))
from systemd_deploy import Deployment
from systemd_common import load_config
from smoke_host import Smoke


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="agentbox-deploy-test-")
        self.addCleanup(self.temporary.cleanup)
        base = Path(self.temporary.name)
        self.source = base / "source"
        self.destination = base / "staged"
        self.destination.mkdir()
        self.app = self.destination / "opt/agentbox"
        self.config = self.destination / "etc/agentbox.env"
        self.state = self.destination / "var/lib/agentbox"
        for name in ("bin/agentboxd", ".local/gvisor/runsc", ".local/gvisor/gvisor-bin/sidecar", ".local/rootfs/usr/local/bin/python3"):
            path = self.source / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o755)
        image = self.source / ".local/rootfs-image.json"
        image.write_text(json.dumps(["python@sha256:" + "1"*64]))
        unit = self.source / "deploy/systemd/agentbox.service"
        unit.parent.mkdir(parents=True)
        unit.write_bytes((REPOSITORY / "deploy/systemd/agentbox.service").read_bytes())

    def deploy(self, *arguments, success=True):
        environment = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
        result = subprocess.run(["bash", "-c", 'umask 0077; exec python3 "$@"', "deploy-test", str(SCRIPT), "--source", str(self.source), "--destdir", str(self.destination), *arguments], env=environment, text=True, capture_output=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def test_install_update_rollback_preserve_configuration_and_state(self):
        result = self.deploy()
        self.assertNotIn("AGENTBOX_API_TOKEN=", result.stdout)
        for path, expected in ((self.app, 0o755), (self.app / "bin/agentboxd", 0o755), (self.app / ".local/rootfs", 0o755), (self.config, 0o600)):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), expected)
        self.assertTrue((self.app / ".local/gvisor/gvisor-bin/sidecar").is_file())
        config = self.config.read_bytes()
        original = (self.app / "bin/agentboxd").read_bytes()
        self.state.mkdir(parents=True)
        marker = self.state / "owner.json"
        marker.write_text('{"retained":true}')
        (self.source / "bin/agentboxd").write_text("#!/bin/sh\nexit 2\n")
        self.deploy("--update")
        self.assertEqual(self.config.read_bytes(), config)
        self.assertEqual(marker.read_text(), '{"retained":true}')
        self.assertNotEqual((self.app / "bin/agentboxd").read_bytes(), original)
        self.deploy("--rollback")
        self.assertEqual((self.app / "bin/agentboxd").read_bytes(), original)
        self.assertEqual(self.config.read_bytes(), config)

    def test_uninstall_retains_data_unless_purge_is_explicit(self):
        self.deploy()
        self.state.mkdir(parents=True)
        (self.state / "owner.json").write_text("retained")
        self.deploy("--uninstall")
        self.assertFalse(self.app.exists())
        self.assertTrue(self.config.exists())
        self.assertTrue(self.state.exists())

    def test_purge_is_explicit_and_scoped(self):
        self.deploy()
        self.state.mkdir(parents=True)
        self.deploy("--uninstall", "--purge")
        self.assertFalse(self.config.exists())
        self.assertFalse(self.state.exists())
        self.assertTrue(self.source.exists())

    def test_dependency_changes_are_rejected_without_modification(self):
        self.deploy()
        original = (self.app / "INSTALL.json").read_bytes()
        (self.source / ".local/gvisor/runsc").write_text("different runtime")
        self.deploy("--update", success=False)
        self.assertEqual((self.app / "INSTALL.json").read_bytes(), original)

    def test_unreconciled_state_blocks_updates(self):
        self.deploy()
        bundles = self.state / "bundles"
        bundles.mkdir(parents=True)
        (bundles / "ab-record").mkdir()
        self.deploy("--update", success=False)
        self.assertTrue((bundles / "ab-record").exists())

    def test_live_state_lock_blocks_updates(self):
        self.deploy()
        self.state.mkdir(parents=True)
        with (self.state / "daemon.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.deploy("--update", success=False)

    def test_unknown_or_symlinked_installations_are_not_overwritten(self):
        outside = self.destination.parent / "outside"
        outside.mkdir()
        (outside / "sentinel").write_text("keep")
        self.app.parent.mkdir()
        self.app.symlink_to(outside, target_is_directory=True)
        self.deploy(success=False)
        self.assertEqual((outside / "sentinel").read_text(), "keep")

    def test_invalid_retained_configuration_is_rejected(self):
        self.config.parent.mkdir()
        self.config.write_text("AGENTBOX_API_TOKEN=too-short\n")
        self.deploy(success=False)
        self.assertFalse(self.app.exists())
        self.assertEqual(self.config.read_text(), "AGENTBOX_API_TOKEN=too-short\n")

    def test_root_aliases_do_not_bypass_live_deployment_checks(self):
        for root in ("/", "//", "/tmp/.."):
            deployment = Deployment(SimpleNamespace(destdir=root, source=str(self.source)))
            self.assertTrue(deployment.live)


class ToolTests(unittest.TestCase):
    def test_normal_stop_requires_success_and_no_retained_records(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            bundles = state / "bundles"
            bundles.mkdir()
            smoke = Smoke.__new__(Smoke)
            with patch("smoke_host.STATE", state), patch("smoke_host.property_value", return_value="success"):
                smoke.assert_normal_stop()
                (bundles / "ab-retained").mkdir()
                with self.assertRaises(ValueError):
                    smoke.assert_normal_stop()
            with patch("smoke_host.property_value", return_value="signal"), self.assertRaises(ValueError):
                smoke.assert_normal_stop()

    def test_lifecycle_rejection_is_reported_without_waiting_for_nonexistent_task(self):
        smoke = Smoke.__new__(Smoke)
        smoke.running_group = lambda root: None
        pending = Future()
        pending.set_result((503, {"error": "executor unavailable"}))
        with self.assertRaisesRegex(ValueError, "HTTP 503"):
            smoke.wait_for_running_task(Path("/unused"), pending)

    def test_configuration_accepts_explicit_loopback_addresses(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            for address in ("127.0.0.1:8080", "[::1]:8080"):
                config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_LISTEN="+address+"\n")
                self.assertEqual(load_config(config)["AGENTBOX_LISTEN"], address)

    def test_configuration_rejects_unsafe_or_ambiguous_values(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            for extra in ("AGENTBOX_LISTEN=0.0.0.0:8080", "AGENTBOX_LISTEN=127.0.0.1:8080/path", "AGENTBOX_LISTEN=127.0.0.1", "AGENTBOX_CONCURRENCY=5", "AGENTBOX_PLATFORM=ptrace", "AGENTBOX_MODE=auto", "AGENTBOX_MAX_SANDBOXES=9", "AGENTBOX_QUEUE_TIMEOUT=6s", "AGENTBOX_WARM_SIZE=7", "AGENTBOX_QUEUE_BYTES=0", "UNKNOWN=value", "AGENTBOX_LISTEN='127.0.0.1:8080'", "AGENTBOX_API_TOKEN="+"y"*32):
                config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\n"+extra+"\n")
                with self.subTest(extra=extra), self.assertRaises(ValueError):
                    load_config(config)

    def test_evidence_contains_versions_and_limits_but_no_token(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "sandboxes").mkdir()
            limits = {"cpu.max":"80000 20000", "memory.high":"3221225472", "memory.max":"4294967296", "memory.swap.max":"0", "pids.max":"2048"}
            for name, value in limits.items():
                (root / "sandboxes" / name).write_text(value)
            smoke = Smoke.__new__(Smoke)
            smoke.config = {"AGENTBOX_PLATFORM":"kvm", "AGENTBOX_API_TOKEN":"private-token-must-not-appear"}
            smoke.manifest = {"format":1, "platform":"kvm", "binary_sha256":"a", "runsc_sha256":"b", "rootfs_images":["python@sha256:fixture"], "unexpected_secret":"private-token-must-not-appear"}
            smoke.results = []
            smoke.runtime_components = ["sentry", "gofer"]
            smoke.sandbox_limits = None
            smoke.lifecycle_checked = True
            smoke.cgroup_root = lambda: root
            smoke.ready = lambda: True
            with patch("smoke_host.systemctl", return_value="systemd 249\nfeatures"), patch("smoke_host.subprocess.run", return_value=SimpleNamespace(stdout="runsc version fixture\n")):
                report = smoke.report()
            self.assertEqual(report["systemd_version"], "systemd 249")
            self.assertEqual(report["parent_limits"], limits)
            self.assertNotIn("private-token-must-not-appear", json.dumps(report))


if __name__ == "__main__":
    unittest.main()
