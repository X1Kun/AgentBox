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

    def prepare_gateway(self):
        (self.source/"bin/agentboxd").write_text("#!/bin/sh\nprintf '%s\\n' '-listen loopback or unix:/absolute/socket'\n")
        self.deploy()
        (self.source/"bin/agentbox-gateway").write_text("#!/bin/sh\nexit 0\n")
        (self.source/"bin/agentbox-gateway").chmod(0o755)
        for name in ("agentbox-gateway.service", "agentbox-gateway-daemon.conf"):
            (self.source/"deploy/systemd"/name).write_bytes((REPOSITORY/"deploy/systemd"/name).read_bytes())

    def gateway(self,*arguments,success=True):
        result=subprocess.run([sys.executable,str(REPOSITORY/"scripts/gateway_deploy.py"),"--source",str(self.source),"--destdir",str(self.destination),*arguments],
                              env=dict(os.environ,PYTHONDONTWRITEBYTECODE="1"),text=True,capture_output=True)
        self.assertEqual(result.returncode==0,success,result.stderr)
        return result

    def test_gateway_install_update_and_remove_preserve_daemon(self):
        self.prepare_gateway()
        original_config=self.config.read_bytes()
        result=self.gateway()
        from systemd_common import load_gateway_config
        config=self.destination/"etc/agentbox-gateway.env"
        backend=self.destination/"etc/agentbox-gateway-daemon.env"
        settings=load_gateway_config(config)
        self.assertEqual(settings["AGENTBOX_GATEWAY_TOKEN"],load_config(self.config)["AGENTBOX_API_TOKEN"])
        self.assertNotEqual(settings["AGENTBOX_GATEWAY_TOKEN"],settings["AGENTBOX_DAEMON_TOKEN"])
        self.assertEqual(stat.S_IMODE(config.stat().st_mode),0o600)
        self.assertEqual(stat.S_IMODE(backend.stat().st_mode),0o600)
        self.assertIn("AGENTBOX_LISTEN=unix:/run/agentbox/daemon.sock",backend.read_text())
        self.assertNotIn(settings["AGENTBOX_DAEMON_TOKEN"],result.stdout+result.stderr)
        self.assertEqual(self.config.read_bytes(),original_config)
        retained=config.read_bytes()
        for operation in ("--update","--rollback","--uninstall"):
            self.deploy(operation,success=False)
        (self.source/"bin/agentbox-gateway").write_text("#!/bin/sh\nexit 2\n")
        self.gateway("--update")
        self.assertEqual(config.read_bytes(),retained)
        self.gateway("--uninstall")
        self.assertFalse((self.app/"GATEWAY_INSTALL.json").exists())
        self.assertFalse(backend.exists())
        self.assertTrue(config.exists())
        self.assertEqual(self.config.read_bytes(),original_config)
        self.gateway() # retained client/internal credentials stay consistent
        self.assertEqual(config.read_bytes(),retained)
        self.gateway("--uninstall","--purge")
        self.assertFalse(config.exists())
        self.deploy("--update")

    def test_gateway_rejects_symlinked_dropin_and_unreconciled_state(self):
        self.prepare_gateway()
        dropin=self.destination/"etc/systemd/system/agentbox.service.d"
        outside=self.destination.parent/"outside-dropin";outside.mkdir()
        dropin.symlink_to(outside,target_is_directory=True)
        self.gateway(success=False)
        self.assertEqual(list(outside.iterdir()),[])
        dropin.unlink()
        bundles=self.state/"bundles";bundles.mkdir(parents=True)
        (bundles/"ab-retained").mkdir()
        self.gateway(success=False)
        self.assertFalse((self.app/"GATEWAY_INSTALL.json").exists())


class ToolTests(unittest.TestCase):
    def test_gateway_configuration(self):
        from systemd_common import load_gateway_config
        with tempfile.TemporaryDirectory() as directory:
            config=Path(directory)/"config"
            base="AGENTBOX_GATEWAY_TOKEN="+"a"*32+"\nAGENTBOX_DAEMON_TOKEN="+"b"*32+"\n"
            config.write_text(base)
            self.assertEqual(load_gateway_config(config)["AGENTBOX_GATEWAY_LISTEN"],"127.0.0.1:8080")
            for extra in ("AGENTBOX_GATEWAY_LISTEN=0.0.0.0:8080", "AGENTBOX_GATEWAY_MAX_INFLIGHT=65", "AGENTBOX_GATEWAY_TIMEOUT=61s"):
                config.write_text(base+extra+"\n")
                with self.subTest(extra=extra),self.assertRaises(ValueError):load_gateway_config(config)
            config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_LISTEN=unix:/run/agentbox/daemon.sock\n")
            self.assertEqual(load_config(config)["AGENTBOX_LISTEN"],"unix:/run/agentbox/daemon.sock")
    def test_dynamic_inventory_configuration_and_unit_wiring(self):
        unit = (REPOSITORY/"deploy/systemd/agentbox.service").read_text()
        settings = {
            "AGENTBOX_WARM_DYNAMIC": ("warm-dynamic", "true"),
            "AGENTBOX_WARM_MIN_SIZE": ("warm-min-size", "1"),
            "AGENTBOX_WARM_MAX_SIZE": ("warm-max-size", "4"),
            "AGENTBOX_WARM_ADJUST_INTERVAL": ("warm-adjust-interval", "100ms"),
            "AGENTBOX_WARM_DOWNSCALE_WINDOW": ("warm-downscale-window", "300ms"),
        }
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory)/"config"
            base = {"AGENTBOX_API_TOKEN": "x"*32, "AGENTBOX_MODE": "warm",
                    **{key: value for key, (_, value) in settings.items()}}
            def write(values):
                config.write_text("".join(key+"="+value+"\n" for key, value in values.items()))
            write(base)
            values = load_config(config)
            for key, (flag, expected) in settings.items():
                self.assertEqual(values[key], expected)
                separator = "=" if flag == "warm-dynamic" else " "
                self.assertIn("-"+flag+separator+"${"+key+"}", unit)
            # Unset maximum inherits warm-size, and the creation worker count
            # is bounded by the dynamic maximum when it is explicitly supplied.
            for extra in ({"AGENTBOX_WARM_MAX_SIZE": "0"},
                          {"AGENTBOX_WARM_SIZE": "1", "AGENTBOX_CREATE_CONCURRENCY": "2"}):
                write({**base, **extra})
                load_config(config)
            for key, value in (("AGENTBOX_MODE", "cold"), ("AGENTBOX_WARM_DYNAMIC", "yes"),
                               ("AGENTBOX_WARM_MIN_SIZE", "5"), ("AGENTBOX_WARM_MIN_SIZE", "0"),
                               ("AGENTBOX_WARM_MAX_SIZE", "7"), ("AGENTBOX_WARM_MAX_SIZE", "-1"),
                               ("AGENTBOX_WARM_ADJUST_INTERVAL", "99ms"),
                               ("AGENTBOX_WARM_DOWNSCALE_WINDOW", "99ms"), ("AGENTBOX_WARM_DOWNSCALE_WINDOW", "3601s")):
                write({**base, key: value})
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    load_config(config)

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

    def test_observe_configuration_keeps_protected_path_explicit(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_QOS=observe\n"
                              "AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/mock-core.service\n"
                              "AGENTBOX_QOS_INTERVAL=50ms\n")
            values = load_config(config)
            self.assertEqual(values["AGENTBOX_QOS"], "observe")
            self.assertEqual(values["AGENTBOX_QOS_INTERVAL"], "50ms")

    def test_configuration_rejects_unsafe_or_ambiguous_values(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            for extra in ("AGENTBOX_LISTEN=0.0.0.0:8080", "AGENTBOX_LISTEN=127.0.0.1:8080/path", "AGENTBOX_LISTEN=127.0.0.1", "AGENTBOX_CONCURRENCY=5", "AGENTBOX_PLATFORM=ptrace", "AGENTBOX_MODE=auto", "AGENTBOX_QOS=enforce", "AGENTBOX_QOS=observe", "AGENTBOX_QOS_INTERVAL=10ms", "AGENTBOX_MAX_SANDBOXES=9", "AGENTBOX_QUEUE_TIMEOUT=6s", "AGENTBOX_WARM_SIZE=7", "AGENTBOX_QUEUE_BYTES=0", "UNKNOWN=value", "AGENTBOX_LISTEN='127.0.0.1:8080'", "AGENTBOX_API_TOKEN="+"y"*32):
                config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\n"+extra+"\n")
                with self.subTest(extra=extra), self.assertRaises(ValueError):
                    load_config(config)

    def test_cpu_control_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory)/"config"
            for mode in ("observe", "enforce"):
                config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_QOS="+mode+"\nAGENTBOX_QOS_CPU_CONTROL=true\nAGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/core\n")
                self.assertEqual(load_config(config)["AGENTBOX_QOS"],mode)
            config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_QOS_CPU_CONTROL=true\n")
            with self.assertRaises(ValueError):load_config(config)

    def test_pressure_scheduling_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            config=Path(directory)/"config"
            for mode in ("observe","enforce"):
                config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_QOS="+mode+"\nAGENTBOX_QOS_SCHEDULING=true\nAGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/core\n")
                self.assertEqual(load_config(config)["AGENTBOX_QOS_SCHEDULING"],"true")
            for mode,enabled in (("off","true"),("observe","yes")):
                config.write_text("AGENTBOX_API_TOKEN="+"x"*32+"\nAGENTBOX_QOS="+mode+"\nAGENTBOX_QOS_SCHEDULING="+enabled+"\nAGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/core\n")
                with self.assertRaises(ValueError):load_config(config)

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
