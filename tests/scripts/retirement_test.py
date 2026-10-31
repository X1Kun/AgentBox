import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest
import json

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('retire_legacy',ROOT/'scripts/retire_legacy.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)

class RetirementTest(unittest.TestCase):
    def fixture(self,root):
        def write(name,data):
            p=root/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(data);return p
        write('opt/agentbox/bin/agentboxd',b'daemon')
        write('opt/agentbox/.local/gvisor/runsc',b'runsc')
        write('opt/agentbox/INSTALL.json',json.dumps({'format':1,'platform':'kvm','binary_sha256':hashlib.sha256(b'daemon').hexdigest(),'runsc_sha256':hashlib.sha256(b'runsc').hexdigest()}).encode())
        write('etc/systemd/system/agentbox.service',b'ExecStart=/opt/agentbox/bin/agentboxd -runsc /opt/agentbox/.local/gvisor/runsc -rootfs /opt/agentbox/.local/rootfs -state-dir /var/lib/agentbox\n')
        write('etc/agentbox.env',b'AGENTBOX_API_TOKEN=private-test-token\n')
        write('var/lib/agentbox/owner.json',json.dumps({'platform':'kvm','rootfs':'/opt/agentbox/.local/rootfs','cgroup_root':'/sys/fs/cgroup/system.slice/agentbox.service'}).encode())
        (root/'var/lib/agentbox/bundles').mkdir()
        (root/'var/lib/agentbox/runsc').mkdir()
        (root/'var/backups').mkdir()
        (root/'run/lock').mkdir(parents=True)
        return module.Retirement(root)

    def test_inspection_and_apply_preserve_unrelated_resources_and_private_backup(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);r=self.fixture(root)
            other=root/'opt/new-platform';other.mkdir();(other/'marker').write_text('keep')
            report={'inspection':r.inspect()};self.assertTrue(r.app.exists())
            events=[];r.apply(report,lambda value:events.append(value['outcome']))
            self.assertEqual(events,['planned','removed']);self.assertFalse(r.app.exists());self.assertFalse(r.state.exists());self.assertTrue((other/'marker').exists())
            self.assertEqual((Path(report['backup'])/'agentbox.env').stat().st_mode & 0o777,0o600)
            self.assertNotIn('private-test-token',json.dumps(report))

    def test_unreconciled_state_changed_binary_and_symlink_are_blocked(self):
        for mode in ('bundle','runsc_record','unknown_state','binary','symlink','new_component','shared_cache'):
            with self.subTest(mode=mode),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);r=self.fixture(root)
                if mode=='bundle':(r.state/'bundles'/'pending').mkdir()
                if mode=='runsc_record':(r.state/'runsc'/'container-state').write_text('preserve')
                if mode=='unknown_state':(r.state/'unrecognized.json').write_text('preserve')
                if mode=='binary':(r.app/'bin/agentboxd').write_bytes(b'changed')
                if mode=='symlink':r.config.unlink();r.config.symlink_to(root/'foreign')
                if mode=='new_component':(r.app/'bin/agentbox-api').write_text('new service')
                if mode=='shared_cache':(r.app/'.local/platform').mkdir()
                with self.assertRaises(ValueError):r.inspect()
                self.assertTrue(r.app.exists())

    def test_failed_audit_stops_before_removal(self):
        with tempfile.TemporaryDirectory() as tmp:
            r=self.fixture(Path(tmp));report={'inspection':r.inspect()}
            def fail(_):raise OSError('disk full')
            with self.assertRaises(OSError):r.apply(report,fail)
            self.assertTrue(r.app.exists());self.assertTrue(r.config.exists());self.assertTrue(r.unit.exists())

if __name__=='__main__':unittest.main()
