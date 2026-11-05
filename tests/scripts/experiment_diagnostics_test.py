"""Bounded collection and immutable failure evidence regressions."""
import json
from pathlib import Path
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/'scripts'))
from lib.observability.diagnostics import archive_observer, archive_metrics, bounded_command, ExperimentDiagnostics, MetricsArchiveError
from lib.acceptance.diagnostics import DiagnosticsAcceptance, batch_fault_isolated
from lib.core.errors import ToolError


class ExperimentEvidence(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def test_observer_archive_filters_window_and_accepts_nanoseconds_on_python310(self):
        source, target = self.directory/'source', self.directory/'archive'
        source.write_text('\n'.join([
            json.dumps({'time': '2026-11-04T17:18:24.228166734+08:00', 'reason': 'timeout'}),
            json.dumps({'time': '2026-11-04T09:18:29.227123690Z', 'reason': 'forbidden'}),
            json.dumps({'time': '2026-11-04T09:20:00Z', 'reason': 'outside'}), 'native unstructured line'])+'\n')
        result = archive_observer(source, target, 1793783900, 1793783920)
        self.assertEqual(result['lines'], 2)
        self.assertEqual(result['skipped_non_json'], 1)
        self.assertNotIn('outside', target.read_text())
        before = target.read_bytes()
        with source.open('a') as stream:
            stream.write('later changes\n')
        self.assertEqual(target.read_bytes(), before)

    def test_missing_and_oversize_observer_logs_are_explicit(self):
        source, target = self.directory/'source', self.directory/'archive'
        self.assertEqual(archive_observer(source, target, 0, 1)['status'], 'missing')
        source.write_bytes(b'x'*((1 << 20)+1))
        with self.assertRaises(ValueError):
            archive_observer(source, target, 0, 1)

    def test_partial_metrics_survive_query_failure_without_becoming_zero(self):
        stack = Mock()
        success = json.dumps({'status': 'success', 'data': {'result': []}})
        stack.http.side_effect = [success, TimeoutError('private native response')] + [success]*30
        target = self.directory/'metrics.json'
        with self.assertRaises(MetricsArchiveError):
            archive_metrics(stack, target, 0, 1)
        result = json.loads(target.read_text())
        self.assertEqual(result['up']['status'], 'success')
        self.assertEqual(result['agentbox_inventory_valid']['status'], 'unavailable')
        self.assertNotIn('private native response', target.read_text())

    def test_diagnostic_subprocesses_obey_time_and_byte_budgets(self):
        with self.assertRaises(ValueError):
            bounded_command([sys.executable, '-c', "import os;os.write(1,b'x'*8192)"], limit=4096)
        before = time.monotonic()
        with self.assertRaises(TimeoutError):
            bounded_command([sys.executable, '-c', 'import time;time.sleep(10)'], timeout=.1)
        self.assertLess(time.monotonic()-before, 3)

    def sampler(self):
        lab = Mock()
        lab.tools = {'kubectl': 'kubectl', 'docker': 'docker'}
        lab.kubeconfig = self.directory/'private-kubeconfig'
        return ExperimentDiagnostics(lab, self.directory)

    def test_controlplane_capture_filters_metrics_and_hides_native_errors(self):
        sampler = self.sampler()
        with patch('lib.observability.diagnostics.bounded_command', side_effect=[
                'apiserver_request_duration_seconds_count{verb="GET"} 4\nunrelated_secret_metric 1\n',
                TimeoutError('Bearer private-token')]):
            sampler.capture_controlplane()
        text = (self.directory/'controlplane-diagnostics.jsonl').read_text()
        self.assertNotIn('private-token', text)
        self.assertNotIn('unrelated_secret_metric', text)
        rows = [json.loads(line) for line in text.splitlines()]
        self.assertEqual(rows[0]['status'], 'sampled')
        self.assertEqual(rows[1]['status'], 'unavailable')

    def test_sampler_worker_failure_is_not_reported_as_complete(self):
        sampler = self.sampler()
        failed = threading.Event()
        def broken():
            failed.set()
            raise OSError('private text')
        def waiting():
            sampler.stop_event.wait()
        sampler.host_loop, sampler.control_loop = broken, waiting
        sampler.start()
        self.assertTrue(failed.wait(1))
        result = sampler.stop()
        self.assertEqual(result['status'], 'incomplete')
        self.assertNotIn('private text', json.dumps(result))
        self.assertFalse(any(t.is_alive() for t in sampler.threads))

    def test_permission_fault_may_also_expire_last_successful_sample(self):
        self.assertTrue(batch_fault_isolated(['batch_inventory_unknown', 'batch_inventory_stale']))
        self.assertFalse(batch_fault_isolated(['session_inventory_unknown', 'batch_inventory_unknown']))
        self.assertFalse(batch_fault_isolated([]))

    def test_empty_rbac_representations_restore_with_uid_and_version_guards(self):
        for shape in ({'rules': []}, {'rules': None}, {}):
            with self.subTest(shape=shape):
                harness = object.__new__(DiagnosticsAcceptance)
                harness.lab = Mock()
                harness.record = {'owner': 'owner'}
                product = Mock()
                product.get.return_value = {**shape, 'metadata': {'uid': 'uid', 'name': 'observer',
                    'resourceVersion': 'version', 'labels': {'agentbox.dev/session': 'owner'}}}
                original = [{'apiGroups': [''], 'resources': ['pods'], 'verbs': ['list']}]
                harness.restore_observer_role(product, 'namespace', {'name': 'observer', 'uid': 'uid'}, original)
                args = harness.lab.kubectl.call_args.args
                operations = json.loads(args[args.index('-p')+1])
                self.assertIn({'op': 'test', 'path': '/metadata/uid', 'value': 'uid'}, operations)
                self.assertIn({'op': 'test', 'path': '/metadata/resourceVersion', 'value': 'version'}, operations)
                self.assertEqual(operations[-1]['value'], original)
                product.get.return_value['rules'] = [{'verbs': ['delete']}]
                harness.lab.kubectl.reset_mock()
                with self.assertRaises(ToolError):
                    harness.restore_observer_role(product, 'namespace', {'name': 'observer', 'uid': 'uid'}, original)
                harness.lab.kubectl.assert_not_called()


if __name__ == '__main__':
    unittest.main()
