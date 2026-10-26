"""Regressions for archive, runtime identity and fail-closed lifecycle acceptance."""
from contextlib import redirect_stdout
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.platform_gvisor import GVisor, GVisorError, RUNTIME, checked_archive, runtime_config
import platform_lab


class GVisorTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.lab = Mock()
        self.lab.config = ROOT / 'deploy/platform'
        self.lab.root = ROOT
        self.lab.runner.output = self.root
        self.runtime = GVisor(self.lab)
        self.runtime.state = {'session': 'a' * 32}
        self.runtime.worker = '1' * 64

    def test_archive_must_match_digest_before_parsing(self):
        path = self.root / 'bad.tar.bz2'
        path.write_bytes(b'not an archive')
        with patch('lib.platform_gvisor.tarfile.open', side_effect=AssertionError('parsed first')):
            with self.assertRaises(GVisorError) as caught:
                checked_archive(path, {'sha512': '0' * 128})
        self.assertEqual(caught.exception.reason, 'gvisor_checksum_mismatch')

    def test_even_verified_archive_must_have_safe_complete_regular_members(self):
        for name, kind, required, ok in [('runsc', tarfile.REGTYPE, ['runsc'], True),
                                         ('runsc', tarfile.REGTYPE, ['runsc', 'shim'], False),
                                         ('../runsc', tarfile.REGTYPE, ['../runsc'], False),
                                         ('runsc', tarfile.SYMTYPE, ['runsc'], False)]:
            with self.subTest(name=name, kind=kind):
                path = self.root / 'sample.tar.bz2'
                with tarfile.open(path, 'w:bz2') as archive:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    if kind == tarfile.REGTYPE:
                        member.size = 4
                        archive.addfile(member, io.BytesIO(b'test'))
                    else:
                        member.linkname = '/etc/passwd'
                        archive.addfile(member)
                lock = {'sha512': hashlib.sha512(path.read_bytes()).hexdigest(), 'required_files': required}
                if ok:
                    self.assertEqual(checked_archive(path, lock), {'runsc': hashlib.sha256(b'test').hexdigest()})
                else:
                    with self.assertRaises(GVisorError):
                        checked_archive(path, lock)

    def test_config_preserves_default_and_matches_schema(self):
        for version in (2, 3, 4):
            original = f'version = {version}\n# original configuration\n'
            result = runtime_config(original)
            self.assertTrue(result.startswith(original))
            self.assertIn('io.containerd.runsc.v1', result)
            self.assertNotIn('default_runtime_name =', result)
            plugin = 'io.containerd.grpc.v1.cri' if version == 2 else 'io.containerd.cri.v1.runtime'
            self.assertIn('plugins."' + plugin + '".containerd.runtimes.agentbox-gvisor', result)
        for original in ('version = 1\n', 'version = 3\n# agentbox-gvisor already installed'):
            with self.assertRaises(GVisorError):
                runtime_config(original)

    def test_runtimeclass_collision_is_never_adopted(self):
        obj = self.runtime.runtime_class()
        changed = json.loads(json.dumps(obj))
        changed['metadata']['labels']['agentbox.dev/session'] = 'b' * 32
        self.lab.kubectl.return_value = json.dumps(changed)
        with self.assertRaises(GVisorError):
            self.runtime.ensure_runtime_class(obj)
        self.assertEqual(self.lab.kubectl.call_count, 1)

    def test_verification_does_not_recreate_removed_runtimeclass(self):
        self.lab.kubectl.return_value = ''
        with self.assertRaises(GVisorError) as caught:
            self.runtime.ensure_runtime_class(self.runtime.runtime_class(), create=False)
        self.assertEqual(caught.exception.reason, 'runtimeclass_missing')
        self.assertEqual(self.lab.kubectl.call_count, 1)

    def test_ready_install_is_verified_without_copy_or_restart(self):
        self.lab.state_dir = self.root
        text = 'version = 2\n'
        (self.root / 'gvisor-containerd.toml').write_text(text)
        h = hashlib.sha256(text.encode()).hexdigest()
        self.runtime.state['gvisor'] = {'phase': 'ready', 'definition': self.runtime.definition(),
                                       'worker_id': self.runtime.worker, 'original_config_sha256': h,
                                       'installed_config_sha256': h}
        with patch('lib.platform_gvisor.checked_archive', return_value={}), \
                patch.object(self.runtime, 'attach'), patch.object(self.runtime, 'node_hash', return_value=h), \
                patch.object(self.runtime, 'verify_installation') as verify, patch.object(self.runtime, 'copy') as copy:
            from contextlib import nullcontext
            self.lab.operation_lock.return_value = nullcontext()
            result = self.runtime.install(self.root / 'unused')
        self.assertTrue(result['already_installed'])
        verify.assert_called_once()
        copy.assert_not_called()
        self.lab.write_state.assert_not_called()

    def test_external_config_change_blocks_install_without_mutation(self):
        self.runtime.state['gvisor'] = {'phase': 'failed', 'definition': self.runtime.definition(),
                                       'worker_id': self.runtime.worker, 'original_config_sha256': 'old',
                                       'installed_config_sha256': 'new'}
        from contextlib import nullcontext
        self.lab.operation_lock.return_value = nullcontext()
        with patch('lib.platform_gvisor.checked_archive', return_value={}), \
                patch.object(self.runtime, 'attach'), patch.object(self.runtime, 'node_hash', return_value='external'), \
                patch.object(self.runtime, 'copy') as copy:
            with self.assertRaises(GVisorError) as caught:
                self.runtime.install(self.root / 'unused')
        self.assertEqual(caught.exception.reason, 'containerd_config_changed')
        copy.assert_not_called()
        self.lab.write_state.assert_not_called()

    def test_worker_operations_require_identity(self):
        self.runtime.worker = None
        with self.assertRaises(GVisorError):
            self.runtime.node('true')
        self.lab.runner.run.assert_not_called()

    def test_runtime_evidence_rejects_runc_even_if_pod_is_running(self):
        pod = {'metadata': {'uid': 'pod-uid'}, 'spec': {'nodeName': self.runtime.worker_name}}
        value = {'items': [{'id': '2' * 64, 'labels': {'io.kubernetes.pod.uid': 'pod-uid'}, 'runtimeHandler': ''}]}
        with patch.object(self.runtime, 'node', return_value=json.dumps(value)), self.assertRaises(GVisorError) as caught:
            self.runtime.runtime_evidence(pod)
        self.assertEqual(caught.exception.reason, 'runtime_evidence_missing')

    def test_runtime_evidence_requires_process_in_the_pod_cgroup(self):
        pod = {'metadata': {'uid': 'pod-uid'}, 'spec': {'nodeName': self.runtime.worker_name}}
        replies = [json.dumps({'items': [{'id': '2' * 64, 'labels': {'io.kubernetes.pod.uid': 'pod-uid'},
                                        'runtimeHandler': RUNTIME}]}), '{}', '{"status":"running"}']
        with patch.object(self.runtime, 'node', side_effect=replies), patch.object(self.runtime, 'processes', return_value=[{'cgroup': '/'}]):
            with self.assertRaises(GVisorError) as caught:
                self.runtime.runtime_evidence(pod)
        self.assertEqual(caught.exception.reason, 'runtime_evidence_missing')

    def test_cleanup_needs_process_and_cri_confirmation_not_just_api_deletion(self):
        record = {'name': 'job', 'uid': 'uid', 'pod_uid': 'pod-uid', 'runtime': {'sandbox_id': '2' * 64}}
        with patch.object(self.runtime, 'get_job'), patch.object(self.runtime, 'pods', return_value=[]), \
                patch.object(self.runtime, 'processes', return_value=[{'pid': 99}]), \
                patch.object(self.runtime, 'node', return_value='{"items":[]}'), \
                patch('lib.platform_gvisor.time.monotonic', side_effect=[0, 0, 61]), patch('lib.platform_gvisor.time.sleep'):
            with self.assertRaises(GVisorError) as caught:
                self.runtime.cleanup(record)
        self.assertEqual(caught.exception.reason, 'gvisor_cleanup_incomplete')
        self.assertFalse(record.get('cleanup_complete'))

    def test_probe_rejects_wrong_job_owner(self):
        record = {'name': 'job', 'uid': 'correct', 'runtime_class': RUNTIME, 'image': 'image'}
        pod = {'metadata': {'labels': {'agentbox.dev/session': 'a' * 32},
                            'ownerReferences': [{'uid': 'wrong', 'controller': True, 'kind': 'Job'}]},
               'spec': {'runtimeClassName': RUNTIME, 'containers': [{'image': 'image'}]}}
        self.lab.kubectl.return_value = json.dumps({'items': [pod]})
        with self.assertRaises(GVisorError) as caught:
            self.runtime.pods(record)
        self.assertEqual(caught.exception.reason, 'probe_identity_changed')

    def test_oom_requires_explicit_oom_reason_not_generic_kill_exit(self):
        record = {'name': 'job', 'uid': 'uid', 'pod_uid': 'pod', 'mode': 'oom'}
        pod = {'metadata': {'uid': 'pod'}, 'status': {'containerStatuses': [
            {'state': {'terminated': {'exitCode': 137, 'reason': 'Error'}}}]}}
        with patch.object(self.runtime, 'get_job', return_value={'status': {'conditions': [{'type': 'Failed', 'status': 'True'}]}}), \
                patch.object(self.runtime, 'pods', return_value=[pod]), patch.object(self.runtime, 'logs', return_value=''), \
                patch.object(self.runtime, 'memory_snapshot', return_value=''):
            with self.assertRaises(GVisorError) as caught:
                self.runtime.wait_terminal(record, 30)
        self.assertEqual(caught.exception.reason, 'unexpected_probe_result')

    def test_oom_accepts_new_kernel_event_when_cri_only_reports_error(self):
        record = {'name': 'job', 'uid': 'uid', 'pod_uid': 'pod', 'mode': 'oom',
                  'runtime': {'resources': [{'cgroup': '/pod', 'limits': 'oom_kill 0\n'},
                                            {'cgroup': '/kubelet.slice', 'limits': 'oom_kill 99\n'}]}}
        pod = {'metadata': {'uid': 'pod'}, 'status': {'containerStatuses': [
            {'state': {'terminated': {'exitCode': 137, 'reason': 'Error'}}}]}}
        with patch.object(self.runtime, 'get_job', return_value={'status': {'conditions': [{'type': 'Failed', 'status': 'True'}]}}), \
                patch.object(self.runtime, 'pods', return_value=[pod]), patch.object(self.runtime, 'logs', return_value=''), \
                patch.object(self.runtime, 'memory_snapshot', return_value='oom_kill 1\n'):
            self.runtime.wait_terminal(record, 30)
        self.assertEqual(record['oom_kill_delta'], 1)

    def test_deadline_accepts_controller_removed_pod_without_inventing_exit_code(self):
        record = {'name': 'job', 'uid': 'uid', 'pod_uid': 'pod', 'mode': 'deadline'}
        status = {'conditions': [{'type': 'Failed', 'status': 'True', 'reason': 'DeadlineExceeded'}]}
        with patch.object(self.runtime, 'get_job', return_value={'status': status}), \
                patch.object(self.runtime, 'pods', return_value=[]):
            self.runtime.wait_terminal(record, 30)
        self.assertIsNone(record['termination'])
        self.assertNotIn('cleanup_complete', record)

    def test_missing_handler_that_executes_is_failure(self):
        pod = {'metadata': {'name': 'pod', 'uid': 'uid'}, 'status': {
            'containerStatuses': [{'state': {'running': {'startedAt': 'now'}}}]}}
        with patch.object(self.runtime, 'ensure_runtime_class'), patch.object(self.runtime, 'job', return_value={}), \
                patch.object(self.runtime, 'pods', return_value=[pod]):
            with self.assertRaises(GVisorError) as caught:
                self.runtime.missing_handler('image', 30)
        self.assertEqual(caught.exception.reason, 'runtime_fallback')

    def test_gvisor_dry_run_does_not_require_tools_or_write_state(self):
        for action in ('gvisor-install', 'gvisor-test'):
            output = self.root / action
            with patch.object(platform_lab.Runner, 'run', side_effect=AssertionError('executed')), redirect_stdout(io.StringIO()):
                self.assertEqual(platform_lab.main([action, '--dry-run', '--output', str(output)]), 0)
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()
