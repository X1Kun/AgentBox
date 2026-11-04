"""Execute bounded fixture programs and compare independent data contracts."""
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.boundary_workloads import compile_workload


class Workloads(unittest.TestCase):
    def test_data_and_expected_failure_match_frozen_contract(self):
        with tempfile.TemporaryDirectory() as tmp:
            Path(tmp, 'marker').write_text('owner:test')
            for kind in ('short', 'memory', 'files', 'output', 'failure'):
                code, expected, exit_code = compile_workload(kind, 'owner:test')
                result = subprocess.run([sys.executable, '-I', '-B', '-c', code], cwd=tmp,
                                        capture_output=True, text=True, timeout=15)
                self.assertEqual((result.returncode, result.stdout), (exit_code, expected), kind)
            code, _, _ = compile_workload('short', 'other-user')
            result = subprocess.run([sys.executable, '-I', '-B', '-c', code], cwd=tmp,
                                    capture_output=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)

class ReadPolling(unittest.TestCase):
    def test_classified_timeout_is_retried_only_as_read(self):
        from unittest.mock import Mock, patch
        from lib.platform_api import APIAcceptance
        api = object.__new__(APIAcceptance)
        api.record = {}
        api.save = Mock()
        api.api = Mock(side_effect=[(504, {'error': 'backend_timeout'}),
                                    (200, {'state': 'ready'})])
        with patch('lib.platform_api.time.sleep'):
            result = api.wait_state('ses-test.uid', 'private', 'ready')
        self.assertEqual(result['state'], 'ready')
        self.assertTrue(all(call.args[0] == 'GET' for call in api.api.call_args_list))


class CreateRecovery(unittest.TestCase):
    def test_same_create_identity_recovers_timeout_without_replaying_data(self):
        from unittest.mock import Mock, patch
        import json
        from lib.platform_api_execution import APIExecutionAcceptance
        api = object.__new__(APIExecutionAcceptance)
        api.record = {'runtime_evidence': {}}
        api.save = Mock()
        view = {'session_id': 'ses-test.uid', 'identity_revision': 'rev', 'state': 'ready'}
        api.api = Mock(side_effect=[(504, {'error': 'backend_timeout'}), (200, view)])
        api.wait_state = Mock(return_value=view)
        api.lab = Mock()
        api.lab.kubectl.side_effect = [json.dumps({'metadata': {'annotations': {'agentbox.dev/api-binding': json.dumps({'pod': {'name': 'pod'}})}}}), json.dumps({'metadata': {'uid': 'pod-uid'}})]
        runtime = Mock()
        runtime.runtime_evidence.return_value = {'sandbox_id': 'sandbox'}
        with patch('lib.platform_api_execution.time.sleep'):
            result = api.session('same-request', 'private', runtime, 'work')
        self.assertEqual(result, view)
        self.assertEqual([call.args[:4] for call in api.api.call_args_list], [('POST', '/v2/sessions', 'private', {'request_id': 'same-request'})]*2)
        self.assertLessEqual(api.wait_state.call_args.args[3], 180)


if __name__ == '__main__':
    unittest.main()
