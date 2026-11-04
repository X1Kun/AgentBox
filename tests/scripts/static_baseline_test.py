"""Baseline scheduling, accounting and teardown regressions without a cluster."""
import json
import copy
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/'scripts'))
from lib import baseline_results as results
from lib.platform_manifests import read_yaml
from lib.static_baseline import BaselineProduct, StaticBaseline
from lib.platform_batch import BatchError


class BaselineResults(unittest.TestCase):
    def setUp(self):
        self.config = read_yaml(ROOT/'deploy/acceptance/boundaries/baseline.yaml')

    def test_plan_pairs_same_trace_without_changing_pool_or_template(self):
        calibration = results.plan(self.config)
        self.assertEqual(len(calibration), 15)
        self.assertTrue(all(c['group'] == 'R' for c in calibration))
        self.assertEqual([c['profile'] for c in calibration[:5]], ['steady-low']*5)
        reference = {'stage': 'calibration', 'config_sha256': results.digest(self.config), 'cleanup_complete': True,
                     'cells': [dict(c, measurement_valid=True, session={'quality_target_met': True}) for c in calibration]}
        plan = results.plan(self.config, 'comparison', reference)
        self.assertEqual(len(plan), 45)
        self.assertEqual(plan, results.plan(self.config, 'comparison', reference))
        for repetition in range(1, 6):
            for profile in self.config['profiles']:
                cells = [c for c in plan if c['repetition'] == repetition and c['profile'] == profile]
                self.assertEqual({c['group'] for c in cells}, {'R', 'B1', 'B2'})
                self.assertEqual(cells[0]['requests'], cells[1]['requests'])
                self.assertEqual(cells[1]['requests'], cells[2]['requests'])

    def test_comparison_requires_valid_repeated_R_reference(self):
        with self.assertRaises(ValueError):
            results.plan(self.config, 'comparison')
        reference = {'stage': 'calibration', 'config_sha256': results.digest(self.config), 'cleanup_complete': True,
            'cells': [dict(c, measurement_valid=True, session={'quality_target_met': c['profile'] == 'steady-low'})
                      for c in results.plan(self.config)]}
        self.assertEqual(results.eligible_profiles(self.config, reference), ['steady-low'])
        self.assertEqual(len(results.plan(self.config, 'comparison', reference)), 15)
        reference['cells'][0]['measurement_valid'] = False
        with self.assertRaises(ValueError):
            results.eligible_profiles(self.config, reference)

    def test_invalid_budget_and_arrivals_are_rejected_before_mutation(self):
        changes = ({'session_pod_budget': 3}, {'repetitions': True}, {'minimum_timely_ratio': 0},
                   {'groups': {'R': 0, 'B1': 2}}, {'batch': {'passes': 64, 'arrivals_ms': [0, 0]}},
                   {'batch': {'passes': 64, 'arrivals_ms': [30000]}})
        for change in changes:
            with self.subTest(change=change), self.assertRaises(ValueError):
                results.plan(dict(self.config, **change))

    def test_failed_dropped_and_late_requests_remain_in_denominator(self):
        trace = {'targets': [{'id': 'session'}], 'requests': [
            {'id': str(i), 'at_ms': 1000*i, 'target': 0} for i in range(4)]}
        events = [{'id': str(i), 'planned_ms': 1000*i, 'session_id': 'session',
                   'sent_ms': 1000*i, 'finished_ms': 1000*i+10, 'outcome': 'correct'} for i in range(4)]
        events[1].update(sent_ms=-1, outcome='dropped')
        events[2]['outcome'] = 'rejected'
        events[3]['sent_ms'] += 500
        events[3]['finished_ms'] += 4000
        summary = results.session_summary(trace, events, self.config)
        self.assertFalse(summary['measurement_valid'])
        self.assertEqual(summary['timely_ratio'], .25)
        self.assertEqual(summary['planned'], 4)
        self.assertEqual(summary['correct'], 2)
        self.assertFalse(summary['quality_target_met'])
        with self.assertRaises(ValueError):
            results.session_summary(trace, events+[events[0]], self.config)

    def test_drain_completion_and_unknown_submission_never_inflate_window_goodput(self):
        summary = results.batch_summary([{'planned_ms': 0, 'verified_at': 4}, {'planned_ms': 1000, 'verified_at': 25}, {'planned_ms': 2000, 'outcome': 'unknown'}], 20, 64)
        self.assertEqual(summary['verified_passes_per_second'], 3.2)
        self.assertEqual(summary['drain_verified'], 1)
        self.assertEqual(summary['unfinished'], 1)
        self.assertEqual(summary['backlog_at_window'], 2)
        self.assertIsNone(summary['completion_upper_bound_seconds'])
        self.assertIsNone(results.percentile([], 99))

    def test_batch_diagnostic_failure_still_deletes_all_owned_resources(self):
        product = object.__new__(BaselineProduct)
        product.state = {'session': 'owner'}
        refs = [{'kind': kind, 'name': kind.lower(), 'uid': 'uid-'+kind}
                for kind in ('Namespace', 'ClusterQueue', 'ResourceFlavor')]
        product.record = {'namespace': 'namespace', 'resources': refs, 'run': 'run'}
        product.runtime = Mock()
        product.runtime.node.side_effect = [ValueError('private details'), json.dumps({'items': []})]
        product.stop = Mock()
        product.save = Mock()
        product._wait_absent = Mock()
        product.lab = Mock()
        product.get = Mock(side_effect=lambda kind, name: {'metadata': {
            'name': name, 'uid': 'uid-'+kind, 'labels': {'agentbox.dev/session': 'owner', 'agentbox.dev/batch-run': 'run'}}})
        with patch('lib.static_baseline.BatchAcceptance') as deletion:
            with self.assertRaises(BatchError):
                product.teardown()
            self.assertEqual(deletion.return_value.delete.call_count, 3)
            self.assertEqual([c.args[0] for c in deletion.return_value.delete.call_args_list],
                             ['Namespace', 'ClusterQueue', 'ResourceFlavor'])
        self.assertFalse(product.record['cleanup_complete'])
        self.assertNotIn('private details', json.dumps(product.record))

    def test_missing_observations_and_stale_timestamps_are_not_idle(self):
        observed = results.observations({}, 100, 120)
        self.assertFalse(observed['inventory_and_scrapes_valid'])
        self.assertEqual(observed['container_costs'], [])
        values = [[100, '1'], [105, '1'], [110, '1'], [115, '1'], [120, '1']]
        series = {'agentbox_inventory_valid': {'data': {'result': [
            {'metric': {'domain': d}, 'values': values} for d in ('session', 'batch')]}},
            'agentbox_inventory_timestamp_seconds': {'data': {'result': [
                {'metric': {'domain': d}, 'values': [[t, str(t-20)] for t, _ in values]} for d in ('session', 'batch')]}}}
        observed = results.observations(series, 100, 120)
        self.assertIn('session_inventory_stale', observed['issues'])

    def test_cost_integrals_do_not_fill_gaps_or_double_count_sources(self):
        metric = {'source': 'cri', 'resource': 'cpu_cores', 'domain': 'session', 'role': 'sandbox'}
        series = {'agentbox_runtime_resource': {'data': {'result': [
            {'metric': metric, 'values': [[100, '1'], [105, '1'], [125, '1']]},
            {'metric': dict(metric, source='cadvisor'), 'values': [[100, '99'], [105, '99']]}]}}}
        costs = results.observations(series, 100, 125)['container_costs']
        self.assertEqual(len(costs), 1)
        self.assertEqual(costs[0]['observed_integral'], 5)
        self.assertEqual(costs[0]['sampled_seconds'], 5)
        self.assertFalse(costs[0]['covers_window'])

    def test_batch_teardown_waits_for_async_cri_reclamation(self):
        product = object.__new__(BaselineProduct)
        product.state = {'session': 'owner'}
        product.record = {'namespace': 'owned', 'resources': [], 'run': 'run'}
        product.runtime = Mock()
        present = json.dumps({'items': [{'id': 'sandbox', 'metadata': {'namespace': 'owned'}}]})
        product.runtime.node.side_effect = [present, present, json.dumps({'items': []})]
        product.runtime.processes.return_value = []
        product.stop = Mock()
        product.save = Mock()
        product.get = Mock(return_value=None)
        with patch('lib.static_baseline.time.sleep') as sleep:
            product.teardown()
        self.assertTrue(product.record['cleanup_complete'])
        self.assertEqual(product.runtime.node.call_count, 3)
        sleep.assert_called_once_with(.5)

    def test_stages_follow_job_owner_uid_and_preserve_attempts(self):
        entries = [{'batch_id': 'job.job-uid', 'planned_ms': 0}]
        snapshots = [{'elapsed_seconds': 2, 'jobs': [{'metadata': {'uid': 'job-uid', 'creationTimestamp': '2026-11-04T00:00:00Z'},
            'status': {'startTime': '2026-11-04T00:00:03Z', 'completionTime': '2026-11-04T00:00:10Z'}}],
            'pods': [{'metadata': {'uid': 'pod-uid', 'name': 'pod', 'ownerReferences': [
                {'kind': 'Job', 'controller': True, 'uid': 'job-uid'}]}, 'status': {'phase': 'Succeeded'}}],
            'workloads.kueue.x-k8s.io': [{'metadata': {'uid': 'workload-uid', 'ownerReferences': [
                {'kind': 'Job', 'controller': True, 'uid': 'job-uid'}]}, 'status': {'conditions': [
                {'type': 'Admitted', 'status': 'True', 'lastTransitionTime': '2026-11-04T00:00:02Z'}]}}]}]
        result = results.batch_stages(snapshots, entries)[0]
        self.assertEqual(result['queue_wait_seconds'], 2)
        self.assertEqual(result['job_run_seconds'], 7)
        self.assertEqual(set(result['attempts']), {'pod-uid'})

    def test_cleanup_rejects_scope_changes_before_contacting_cluster(self):
        with tempfile.TemporaryDirectory() as tmp:
            report = Path(tmp)/'static-baseline.json'
            record = {'run': 'a'*10, 'owner': 'owner', 'namespaces': {
                'work': {'name': 'unrelated-namespace', 'uid': 'uid'}}}
            report.write_text(json.dumps(record))
            harness = object.__new__(StaticBaseline)
            harness.lab = Mock()
            with self.assertRaises(BatchError):
                harness.cleanup(report)
            harness.lab.operation_lock.assert_not_called()
            record['namespaces'] = {}
            report.write_text(json.dumps(record))
            (Path(tmp)/'batch-product-test.json').write_text(json.dumps({
                'run': 'a'*10, 'owner': 'owner', 'namespace': 'agentbox-batch-product-'+'a'*10,
                'resources': [{'kind': 'ClusterQueue', 'name': 'unrelated-queue', 'uid': 'uid'}]}))
            with self.assertRaises(BatchError):
                harness.cleanup(report)
            harness.lab.operation_lock.assert_not_called()

    def test_quota_change_waits_for_controller_observed_generation(self):
        product = object.__new__(BaselineProduct)
        product.state = {'session': 'owner'}
        product.record = {'namespace': 'queue', 'run': 'run', 'resources': [{'kind': 'ClusterQueue', 'uid': 'queue-uid'}]}
        old = {'metadata': {'name': 'queue', 'uid': 'queue-uid', 'generation': 1, 'resourceVersion': '10',
                           'labels': {'agentbox.dev/session': 'owner', 'agentbox.dev/batch-run': 'run'}},
               'status': {'conditions': [{'type': 'Active', 'status': 'True', 'observedGeneration': 1}]}}
        updated = copy.deepcopy(old)
        updated['metadata']['generation'] = 2
        ready = copy.deepcopy(updated)
        ready['status']['conditions'][0]['observedGeneration'] = 2
        product.get = Mock(side_effect=[old, updated, ready])
        product.lab = Mock()
        product.lab.kubectl.return_value = json.dumps(updated)
        product.save = Mock()
        with patch('lib.static_baseline.time.sleep') as sleep:
            product.quota(2)
        sleep.assert_called_once_with(.5)
        self.assertEqual(product.record['effective_queue']['status']['conditions'][0]['observedGeneration'], 2)


if __name__ == '__main__':
    unittest.main()
