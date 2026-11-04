"""Failures in diagnostics must never bypass owned namespace teardown."""
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.session_boundaries import SessionBoundaries
from lib.platform_batch import BatchError


class BoundaryTeardown(unittest.TestCase):
    def setup_harness(self):
        harness = object.__new__(SessionBoundaries)
        harness.record = {'all_passed': True}
        harness.namespaces = {'work': {'name': 'work', 'uid': 'work-uid'},
                              'client': {'name': 'client', 'uid': 'client-uid'}}
        harness.lab = Mock()
        harness.lab.kubectl.return_value = json.dumps({'items': [{
            'metadata': {'uid': 'pod-uid'}, 'spec': {'runtimeClassName': 'agentbox-gvisor'},
            'status': {'phase': 'Running'}}]})
        harness.stack = Mock()
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        harness.output = Path(temporary.name)
        harness.stack.directory = Path(temporary.name)/'observer'
        harness.forwards = {'api': Mock()}
        harness.save = Mock()
        api = Mock(record={})
        sessions = Mock()
        runtime = Mock()
        runtime.runtime_evidence.return_value = {'sandbox_id': 'sandbox-id'}
        runtime.node.return_value = json.dumps({'items': [
            {'id': 'sandbox-id', 'metadata': {'namespace': 'work'}}]})
        runtime.processes.return_value = []
        return harness, api, sessions, runtime

    def test_running_pod_with_missing_cri_still_deletes_both_namespaces(self):
        h, api, sessions, runtime = self.setup_harness()
        runtime.runtime_evidence.side_effect = ValueError('private native response')
        with self.assertRaises(BatchError):
            h._teardown(api, sessions, runtime, {}, 'run')
        self.assertEqual(sessions.close_namespaces.call_count, 2)
        self.assertEqual({next(iter(c.args[0])) for c in sessions.close_namespaces.call_args_list}, {'work', 'client'})
        self.assertTrue(h.record['namespace_cleanup_complete'])
        self.assertFalse(h.record['runtime_cleanup_complete'])
        self.assertFalse(h.record['cleanup_complete'])
        self.assertFalse(h.record['all_passed'])
        self.assertIn({'stage': 'runtime_evidence', 'type': 'ValueError', 'reason': None}, h.record['teardown_errors'])
        self.assertNotIn('private native response', json.dumps(h.record))
        h.save.assert_called_once()

    def test_other_teardown_failures_do_not_skip_namespace_deletion(self):
        for stage in ('archive', 'metrics', 'observer_archive', 'api', 'observer', 'targets', 'forward', 'pods', 'cri', 'processes'):
            with self.subTest(stage=stage):
                h, api, sessions, runtime = self.setup_harness()
                fail = OSError('private credential details')
                if stage == 'archive':
                    h._capture_diagnostics = Mock(side_effect=fail)
                elif stage == 'metrics':
                    h._capture_metrics = Mock(side_effect=fail)
                elif stage == 'observer_archive':
                    h._archive_observer = Mock(side_effect=fail)
                elif stage == 'api':
                    api.stop.side_effect = fail
                elif stage == 'observer':
                    h.stack.stop_observer.side_effect = fail
                elif stage == 'targets':
                    h.stack.targets.side_effect = fail
                elif stage == 'forward':
                    h.forwards['api'].close.side_effect = fail
                elif stage == 'pods':
                    h.lab.kubectl.side_effect = fail
                elif stage == 'cri':
                    runtime.node.side_effect = fail
                else:
                    runtime.processes.side_effect = fail
                with self.assertRaises(BatchError):
                    h._teardown(api, sessions, runtime, {}, 'run')
                self.assertEqual(sessions.close_namespaces.call_count, 2)
                self.assertTrue(h.record['namespace_cleanup_complete'])
                self.assertFalse(h.record['cleanup_complete'])
                self.assertTrue(h.record['teardown_errors'])
                self.assertNotIn('private credential details', json.dumps(h.record))
                h.save.assert_called_once()

    def test_one_namespace_failure_does_not_skip_the_other(self):
        h, api, sessions, runtime = self.setup_harness()
        sessions.close_namespaces.side_effect = [ValueError('UID replaced'), None]
        with self.assertRaises(BatchError):
            h._teardown(api, sessions, runtime, {}, 'run')
        self.assertEqual(sessions.close_namespaces.call_count, 2)
        self.assertFalse(h.record['namespace_cleanup_complete'])
        self.assertTrue(h.record['runtime_cleanup_complete'])
        self.assertFalse(h.record['cleanup_complete'])

    def test_success_retains_independent_namespace_and_runtime_confirmation(self):
        h, api, sessions, runtime = self.setup_harness()
        h._teardown(api, sessions, runtime, {}, 'run')
        self.assertTrue(h.record['namespace_cleanup_complete'])
        self.assertTrue(h.record['runtime_cleanup_complete'])
        self.assertTrue(h.record['cleanup_complete'])
        self.assertEqual(h.record['remaining_processes'], {'sandbox-id': []})
        self.assertEqual(h.record['teardown_errors'], [])


if __name__ == '__main__':
    unittest.main()
