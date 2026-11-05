"""Batch/Kueue ownership, quota, attempt and reclamation regressions; offline."""
from contextlib import nullcontext, redirect_stdout
import copy
import errno
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

import yaml

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.acceptance.batch import BatchAcceptance, validate_cleanup_record
from lib.core.errors import ToolError
from lib.platform.kueue import Kueue, OWNER, RUN, MANAGED, checked_manifest, controller_source, render_controller
from lib.core.kubernetes import identity, quantity
from lib.platform.kueue import webhook_contract
from lib.manifests import objects, render
from lib.core.errors import SessionError
import importlib.util
spec=importlib.util.spec_from_file_location("batch_guest_probe",ROOT/"tests/fixtures/probes/batch_guest_probe.py")
batch_guest_probe=importlib.util.module_from_spec(spec)
spec.loader.exec_module(batch_guest_probe)
import platform_lab


def upstream(version):
    return [
        {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'kueue-system'}},
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'kueue-manager-config', 'namespace': 'kueue-system'},
         'data': {'controller_manager_config.yaml': 'old'}},
        {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': 'kueue-controller-manager', 'namespace': 'kueue-system'},
         'spec': {'selector': {'matchLabels': {'app': 'kueue'}}, 'template': {'metadata': {'labels': {'app': 'kueue'}},
            'spec': {'containers': [{'name': 'manager', 'image': 'registry.k8s.io/kueue/kueue:' + version,
                                    'imagePullPolicy': 'Always', 'resources': {}}]}}}},
        *[{'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': kind,
           'metadata': {'name': 'kueue-' + name + '-webhook-configuration'}, 'webhooks': []}
          for kind, name in [('MutatingWebhookConfiguration', 'mutating'), ('ValidatingWebhookConfiguration', 'validating')]]
    ]


class BatchTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.output = Path(self.temp.name)
        self.lab = Mock()
        self.lab.root, self.lab.config = ROOT, ROOT / 'deploy'
        self.lab.runner.output = self.output
        self.lab.operation_lock.return_value = nullcontext()
        self.kueue = Kueue(self.lab)
        self.owner = 'a' * 32
        self.kueue.state = {'session': self.owner, 'cluster': 'agentbox-platform-lab'}
        self.batch = BatchAcceptance(self.lab)
        self.batch.state = self.kueue.state
        self.batch.runtime = Mock()
        self.batch.runtime.worker_name = 'agentbox-platform-lab-worker'
        self.batch.record = {'schema_version': 1, 'owner': self.owner, 'run': 'b' * 10,
            'namespace': 'agentbox-batch-' + 'b' * 10, 'cluster_queue': 'agentbox-batch-' + 'b' * 10,
            'flavor': 'agentbox-batch-worker-' + 'b' * 10, 'resources': [], 'jobs': {},
            'image': platform_lab.python_image_source(ROOT / 'deploy'), 'cleanup_complete': False}

    def resource(self, kind='Job', uid='job-uid', name='test'):
        return {'apiVersion': 'batch/v1', 'kind': kind, 'metadata': {'name': name, 'uid': uid,
            'labels': {OWNER: self.owner, RUN: self.batch.record['run'], 'kueue.x-k8s.io/queue-name': 'batch'}}}

    def job_record(self):
        manifest = render(ROOT / 'deploy', 'batch-job', dict(self.batch.params(), NAME='test', IMAGE=self.batch.record['image']))
        return {'name': 'test', 'uid': 'job-uid', 'mode': 'success', 'manifest': manifest, 'workload': None,
                'pod': None, 'cleanup_complete': False}

    def test_manifest_checksum_is_rejected_before_any_cluster_access(self):
        path = self.output / 'wrong.yaml'
        path.write_text('wrong')
        with self.assertRaises(ToolError) as caught:
            self.kueue.install(path)
        self.assertEqual(caught.exception.reason, 'kueue_checksum_mismatch')
        self.lab.owned.assert_not_called()
        self.lab.kubectl.assert_not_called()
        self.lab.runner.run.assert_not_called()

    def test_attach_passes_owned_state_to_native_control_plane_check(self):
        state = self.kueue.state
        with patch('lib.platform.kueue.Network') as network, patch('lib.platform.kueue.Sessions') as sessions, \
                patch('lib.platform.kueue.GVisor') as runtime:
            network.return_value.state = state
            self.assertIs(self.kueue.attach(), runtime.return_value)
            self.assertIs(sessions.return_value.state, state)
            sessions.return_value.control_plane_ready.assert_called_once_with()
            runtime.return_value.verify_installation.assert_called_once_with()

    def test_manifest_requires_release_count_and_exact_controller_image(self):
        values = upstream(self.kueue.lock['version'])
        path = self.output / 'upstream.yaml'
        for changed in (False, True):
            current = copy.deepcopy(values)
            if changed:
                current[2]['spec']['template']['spec']['containers'][0]['image'] = 'unlocked:latest'
            path.write_text(yaml.safe_dump_all(current))
            lock = dict(self.kueue.lock, object_count=len(values), manifest_sha256=hashlib.sha256(path.read_bytes()).hexdigest())
            if changed:
                with self.assertRaisesRegex(ToolError, 'controller image'):
                    checked_manifest(path, lock)
            else:
                self.assertEqual(checked_manifest(path, lock), current)

    def test_render_limits_frameworks_and_webhook_scope_without_global_namespace_transform(self):
        values = upstream(self.kueue.lock['version'])
        before = copy.deepcopy(values)
        result = objects(render_controller(ROOT / 'deploy', values, self.owner, self.kueue.image_source()))
        self.assertEqual(values, before)
        cm = next(v for v in result if v['kind'] == 'ConfigMap')
        config = yaml.safe_load(cm['data']['controller_manager_config.yaml'])
        self.assertEqual(config['integrations']['frameworks'], ['batch/job'])
        self.assertFalse(config['manageJobsWithoutQueueName'])
        self.assertEqual(config['managedJobsNamespaceSelector'], {'matchLabels': {MANAGED: 'true'}})
        self.assertTrue(all(v['metadata']['labels'][OWNER] == self.owner for v in result))
        for obj in result:
            if 'webhooks' not in obj:
                continue
            for hook in obj['webhooks']:
                for rule in hook['rules']:
                    self.assertTrue(rule['apiGroups'] == ['kueue.x-k8s.io'] or
                                    (rule['apiGroups'] == ['batch'] and rule['resources'] == ['jobs']))
                    if rule['apiGroups'] == ['batch'] or rule['resources'][0].startswith('workloads'):
                        self.assertEqual(hook['namespaceSelector'], {'matchLabels': {MANAGED: 'true'}})
        deployment = next(v for v in result if v['kind'] == 'Deployment')
        self.assertEqual(deployment['spec']['selector'], before[2]['spec']['selector'])
        self.assertEqual(deployment['spec']['template']['spec']['containers'][0]['image'], self.kueue.image_source())

    def test_controller_mirror_must_preserve_locked_digest(self):
        original = self.kueue.lock['controller_image']
        supplied = 'mirror.example/kueue@' + original.split('@')[1]
        self.assertEqual(controller_source(ROOT / 'deploy', supplied), supplied)
        with self.assertRaises(SessionError):
            controller_source(ROOT / 'deploy', 'mirror.example/kueue@sha256:' + '0' * 64)

    def test_inventory_reads_metadata_only_and_rejects_foreign_ownership(self):
        self.lab.kubectl.return_value = 'v1\tSecret\tkueue-system\tcert\tsecret-uid\t{"agentbox.dev/session":"foreign"}\n'
        with self.assertRaises(ToolError) as caught:
            self.kueue.inventory([{'kind': 'Secret', 'metadata': {'name': 'cert'}}])
        self.assertEqual(caught.exception.reason, 'unowned_kueue_resource')
        args = self.lab.kubectl.call_args.args
        self.assertIn('jsonpath=', args[-1])
        self.assertNotIn('.data', args[-1])
        self.assertNotIn('-o=json', args)

    def test_webhook_defaults_do_not_hide_scope_or_failure_policy_changes(self):
        hook = yaml.safe_load((ROOT / 'deploy/components/kueue/mutating-lab-patch.yaml').read_text())[0]['value'][1]
        actual = copy.deepcopy(hook)
        actual['clientConfig']['service']['port'] = 443
        actual['clientConfig']['caBundle'] = 'public-serving-ca'
        actual['rules'][0]['scope'] = '*'
        actual['objectSelector'] = {}
        self.assertEqual(webhook_contract(hook), webhook_contract(actual))
        for key, value in [('namespaceSelector', {}), ('failurePolicy', 'Ignore')]:
            changed = copy.deepcopy(actual)
            changed[key] = value
            self.assertNotEqual(webhook_contract(hook), webhook_contract(changed))

    def test_ready_install_verifies_without_apply_or_restarting_controller(self):
        self.kueue.state['kueue'] = {'phase': 'ready', 'definition': self.kueue.definition(), 'image': self.kueue.image_source()}
        with patch('lib.platform.kueue.checked_manifest', return_value=[]), patch.object(self.kueue, 'attach'), \
                patch('lib.platform.kueue.render_controller', return_value={'kind': 'List', 'items': []}), \
                patch.object(self.kueue, 'verify', return_value={'verified': True}):
            self.assertTrue(self.kueue.install(self.output / 'unused')['already_installed'])
        self.lab.kubectl.assert_not_called()
        self.lab.runner.run.assert_not_called()
        self.lab.write_state.assert_not_called()

    def test_ready_install_refuses_changed_source_instead_of_upgrading(self):
        self.kueue.state['kueue'] = {'phase': 'ready', 'definition': self.kueue.definition(), 'image': 'old-source'}
        with patch('lib.platform.kueue.checked_manifest', return_value=[]), patch.object(self.kueue, 'attach'), \
                patch('lib.platform.kueue.render_controller', return_value={'kind': 'List', 'items': []}):
            with self.assertRaises(ToolError) as caught:
                self.kueue.install(self.output / 'unused')
        self.assertEqual(caught.exception.reason, 'kueue_definition_changed')
        self.lab.kubectl.assert_not_called()

    def test_uid_replacement_blocks_component_verification(self):
        self.kueue.state['kueue'] = {'phase': 'ready', 'definition': self.kueue.definition(), 'resources': [], 'uids': {'object': 'old'}}
        with patch.object(self.kueue, 'inventory', return_value={'object': 'new'}):
            with self.assertRaises(ToolError) as caught:
                self.kueue.verify()
        self.assertEqual(caught.exception.reason, 'kueue_identity_changed')

    def test_batch_namespace_flavor_queue_and_budget_links_remain_consistent(self):
        values = objects(render(ROOT / 'deploy', 'batch-resources', self.batch.params()))
        namespace = next(v for v in values if v['kind'] == 'Namespace')
        queue = next(v for v in values if v['kind'] == 'ClusterQueue')
        local = next(v for v in values if v['kind'] == 'LocalQueue')
        flavor = next(v for v in values if v['kind'] == 'ResourceFlavor')
        self.assertEqual(namespace['metadata']['name'], self.batch.record['namespace'])
        self.assertEqual(queue['spec']['namespaceSelector']['matchLabels'][RUN], self.batch.record['run'])
        self.assertEqual(local['spec']['clusterQueue'], queue['metadata']['name'])
        self.assertEqual(queue['spec']['resourceGroups'][0]['flavors'][0]['name'], flavor['metadata']['name'])
        self.assertFalse(queue['spec'].get('cohortName'))
        self.assertEqual(flavor['spec']['nodeLabels']['kubernetes.io/hostname'], self.batch.runtime.worker_name)
        job = self.job_record()['manifest']
        self.assertTrue(job['spec']['suspend'])
        self.assertEqual(job['spec']['backoffLimit'], 0)
        self.assertEqual(job['spec']['template']['spec']['runtimeClassName'], 'agentbox-gvisor')
        self.assertFalse(job['spec']['template']['spec']['automountServiceAccountToken'])
        self.assertEqual(job['spec']['template']['spec']['containers'][0]['env'][1]['value'], self.batch.record['run'])

    def test_quantity_arithmetic_and_capacity_include_session_limits(self):
        self.assertEqual(quantity('500m') * 2, quantity('1'))
        self.assertEqual(quantity('256Mi') * 4, quantity('1Gi'))
        values = objects(render(ROOT / 'deploy', 'batch-resources', self.batch.params()))
        self.lab.kubectl.side_effect = [json.dumps({'items': [{'metadata': {'name': self.batch.runtime.worker_name},
            'status': {'allocatable': {'cpu': '1', 'memory': '1Gi'}}}]}), '{"items":[]}']
        with self.assertRaises(ToolError) as caught:
            self.batch.budget(values)
        self.assertEqual(caught.exception.reason, 'batch_capacity_insufficient')

    def test_guest_requires_readonly_mount_and_denied_write_independently(self):
        with patch.object(batch_guest_probe.os, 'getuid', return_value=1000), \
                patch.object(batch_guest_probe.os, 'statvfs') as stat, \
                patch.object(batch_guest_probe.Path, 'write_text') as write, \
                patch.object(batch_guest_probe.Path, 'exists', return_value=False):
            stat.return_value = SimpleNamespace(f_flag=batch_guest_probe.os.ST_RDONLY)
            write.side_effect = OSError(errno.EACCES, 'non-root access denied')
            self.assertTrue(batch_guest_probe.security()['readonly_root'])
            stat.return_value = SimpleNamespace(f_flag=0)
            with self.assertRaises(RuntimeError):
                batch_guest_probe.security()
            stat.return_value = SimpleNamespace(f_flag=batch_guest_probe.os.ST_RDONLY)
            write.side_effect = None
            with self.assertRaises(RuntimeError):
                batch_guest_probe.security()

    def test_mismatched_queue_budget_fails_before_node_access(self):
        values = objects(render(ROOT / 'deploy', 'batch-resources', self.batch.params()))
        next(v for v in values if v['kind'] == 'ResourceQuota')['spec']['hard']['limits.cpu'] = '2'
        with self.assertRaises(ToolError) as caught:
            self.batch.budget(values)
        self.assertEqual(caught.exception.reason, 'invalid_batch_budget')
        self.lab.kubectl.assert_not_called()

    def test_job_uid_and_owner_changes_are_rejected(self):
        record = self.job_record()
        for key, value in [('uid', 'replacement'), ('labels', {OWNER: 'foreign', RUN: self.batch.record['run']})]:
            obj = self.resource()
            obj['metadata'][key] = value
            with patch.object(self.batch, 'get', return_value=obj), self.assertRaises(ToolError):
                self.batch.job(record)

    def test_workload_is_matched_by_job_uid_not_name(self):
        record = self.job_record()
        obj = self.resource('Workload', 'workload-uid')
        obj['metadata']['ownerReferences'] = [{'uid': 'other-job', 'kind': 'Job', 'controller': True}]
        obj['spec'] = {'queueName': 'batch'}
        self.lab.kubectl.return_value = json.dumps({'items': [obj]})
        self.assertIsNone(self.batch.workload(record))
        obj['metadata']['ownerReferences'][0]['uid'] = record['uid']
        self.lab.kubectl.return_value = json.dumps({'items': [obj, copy.deepcopy(obj)]})
        with self.assertRaises(ToolError) as caught:
            self.batch.workload(record)
        self.assertEqual(caught.exception.reason, 'duplicate_batch_workload')

    def test_pods_reject_wrong_runtime_budget_or_job_attempt(self):
        record = self.job_record()
        pod = self.resource('Pod', 'pod-uid')
        pod['metadata']['ownerReferences'] = [{'uid': record['uid'], 'kind': 'Job', 'controller': True}]
        pod['spec'] = copy.deepcopy(record['manifest']['spec']['template']['spec'])
        mutations = [('runtimeClassName', 'runc'), ('nodeName', 'another-node'), ('automountServiceAccountToken', True)]
        for key, value in mutations:
            changed = copy.deepcopy(pod)
            changed['spec'][key] = value
            self.lab.kubectl.return_value = json.dumps({'items': [changed]})
            with self.subTest(field=key), self.assertRaises(ToolError):
                self.batch.pods(record)
        record['pod'] = {'uid': 'previous-pod'}
        self.lab.kubectl.return_value = json.dumps({'items': [pod]})
        with self.assertRaises(ToolError):
            self.batch.pods(record)

    def test_pending_job_cannot_count_as_waiting_if_a_pod_was_created(self):
        record = self.job_record()
        job = self.resource()
        job['spec'] = {'suspend': True}
        with patch.object(self.batch, 'job', return_value=job), patch.object(self.batch, 'workload', return_value=None), \
                patch.object(self.batch, 'pods', return_value=[{'already': 'started'}]):
            with self.assertRaises(ToolError) as caught:
                self.batch.pending(record)
        self.assertEqual(caught.exception.reason, 'unexpected_batch_admission')

    def test_activation_exit_race_is_recorded_without_replay_or_claiming_completion(self):
        record = self.job_record()
        pod = {'metadata': {'name': 'test-pod'}}
        self.lab.runner.commands = [{'exit_code': 137}]
        self.lab.kubectl.side_effect = platform_lab.CommandError('command_failed', 'exec exited 137')
        self.batch.activate(record, pod)
        self.lab.kubectl.assert_called_once()
        self.assertEqual(record['activation_transport_error']['exit_code'], 137)
        self.assertFalse(record['cleanup_complete'])
        self.assertNotIn('termination', record)

    def test_other_activation_errors_remain_failures(self):
        record = self.job_record()
        self.lab.runner.commands = [{'exit_code': 1}]
        self.lab.kubectl.side_effect = platform_lab.CommandError('command_failed', 'permission failure')
        with self.assertRaises(platform_lab.CommandError):
            self.batch.activate(record, {'metadata': {'name': 'test-pod'}})
        self.lab.kubectl.assert_called_once()

    def test_cleanup_uses_server_side_uid_preconditions(self):
        self.batch.delete('Job', 'test', 'original-uid', self.batch.record['namespace'])
        call = self.lab.kubectl.call_args
        self.assertIn('--raw', call.args)
        self.assertEqual(json.loads(call.kwargs['input_text'])['preconditions'], {'uid': 'original-uid'})
        self.assertEqual(json.loads(call.kwargs['input_text'])['propagationPolicy'], 'Foreground')

    def test_cleanup_refuses_replaced_job_before_delete(self):
        record = self.job_record()
        with patch.object(self.batch, 'get', return_value=self.resource(uid='replacement')), \
                patch.object(self.batch, 'delete') as delete:
            with self.assertRaises(ToolError):
                self.batch.cleanup_job(record)
        delete.assert_not_called()

    def test_api_deletion_does_not_mean_runtime_reclamation(self):
        record = self.job_record()
        record['runtime'] = {'sandbox_id': '1' * 64}
        self.batch.runtime.node.return_value = '{"items":[]}'
        self.batch.runtime.processes.return_value = [{'pid': 123}]
        def wait(predicate, *args, **kwargs):
            self.assertFalse(predicate())
            raise ToolError('batch_observation_timeout', 'runtime remains')
        with patch.object(self.batch, 'get', return_value=None), patch.object(self.batch, 'pods', return_value=[]), \
                patch.object(self.batch, 'workloads', return_value=[]), patch.object(self.batch, 'wait', side_effect=wait):
            with self.assertRaises(ToolError):
                self.batch.cleanup_job(record)
        self.assertFalse(record['cleanup_complete'])

    def test_bad_cleanup_report_is_rejected_before_cluster_access(self):
        path = self.output / 'bad-report.json'
        path.write_text(json.dumps(dict(self.batch.record, namespace='unrelated')))
        with self.assertRaises(ToolError):
            self.batch.cleanup(path)
        self.lab.owned.assert_not_called()
        self.lab.kubectl.assert_not_called()

    def test_cleanup_report_cannot_expand_to_other_cluster_resources(self):
        value = copy.deepcopy(self.batch.record)
        value['resources'] = [{'kind': 'ClusterQueue', 'name': 'unrelated', 'uid': 'some-uid'}]
        with self.assertRaises(ToolError):
            validate_cleanup_record(value)
        value['resources'] = []
        record = self.job_record()
        record['manifest']['metadata']['namespace'] = 'unrelated'
        value['jobs'] = {'test': record}
        with self.assertRaises(ToolError):
            validate_cleanup_record(value)

    def test_cleanup_snapshots_sandbox_before_deletion_even_without_runtime_evidence(self):
        record = self.job_record()
        record['pod'] = {'name': 'test-pod', 'uid': 'pod-uid'}
        events = []
        self.batch.runtime.node.side_effect = lambda *args, **kwargs: (
            events.append('snapshot') or json.dumps({'items': [{'id': '1' * 64, 'labels': {'io.kubernetes.pod.uid': 'pod-uid'}}]}))
        def delete(*args):
            events.append('delete')
            self.assertEqual(record['cleanup_sandbox_ids'], ['1' * 64])
        with patch.object(self.batch, 'get', return_value=self.resource()), patch.object(self.batch, 'pods', return_value=[]), \
                patch.object(self.batch, 'delete', side_effect=delete), patch.object(self.batch, 'wait'):
            self.batch.cleanup_job(record)
        self.assertEqual(events, ['snapshot', 'delete'])

    def test_batch_dry_runs_are_offline_and_use_fixed_digest(self):
        for action in ('batch-install', 'batch-test', 'batch-cleanup'):
            args = [action, '--dry-run'] + (['--batch-report', '/not/read.json'] if action == 'batch-cleanup' else [])
            with patch.object(platform_lab.Runner, 'run', side_effect=AssertionError('executed')):
                output = io.StringIO()
                with redirect_stdout(output):
                    self.assertEqual(platform_lab.main(args), 0)
                value = json.loads(output.getvalue())
                self.assertEqual(value['kueue']['api_version'], 'kueue.x-k8s.io/v1beta2')
                self.assertEqual(value['kueue_image'].split('@')[1], self.kueue.lock['controller_image'].split('@')[1])


if __name__ == '__main__':
    unittest.main()
