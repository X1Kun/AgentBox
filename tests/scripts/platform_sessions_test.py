"""Cold-session installer ownership and evidence regressions; no cluster needed."""
from contextlib import nullcontext
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.platform_sessions import Sessions, SessionError, checked_source, render_controller, warm_fixture, OWNER, RUN, ROLE


class SessionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.lab = Mock()
        self.lab.config = ROOT / 'deploy/platform'
        self.lab.root = ROOT
        self.lab.state_dir = self.root
        self.lab.tools = {'kind': 'kind', 'docker': 'docker'}
        self.lab.runner.output = self.root
        self.lab.operation_lock.return_value = nullcontext()
        self.sessions = Sessions(self.lab)
        self.sessions.state = {'session': 'a' * 32, 'cluster': 'owned'}

    def test_source_requires_exact_digest(self):
        official = self.sessions.lock['controller_image']
        mirror = 'mirror.example/controller@' + official.split('@')[1]
        self.assertEqual(checked_source(official, mirror), mirror)
        for value in ('mirror:latest', official[:-1] + ('0' if official[-1] != '0' else '1'), 'https://' + mirror):
            with self.subTest(value=value), self.assertRaises(SessionError):
                checked_source(official, value)

    def test_checksum_rejected_before_cluster_or_docker_access(self):
        path = self.root / 'release.yaml'
        path.write_text('unexpected')
        with self.assertRaises(SessionError) as caught:
            self.sessions.install(path)
        self.assertEqual(caught.exception.reason, 'session_manifest_checksum_mismatch')
        self.lab.kubectl.assert_not_called()
        self.lab.runner.run.assert_not_called()
        self.lab.write_state.assert_not_called()

    def controller(self):
        return {'kind': 'Deployment', 'metadata': {'name': 'agent-sandbox-controller', 'namespace': 'agent-sandbox-system'},
                'spec': {'selector': {'matchLabels': {'app': 'upstream'}}, 'template': {'spec': {'containers': [{
                    'name': 'controller', 'image': 'registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.8',
                    'args': ['--leader-elect=true', '--extensions']}]}}}}

    def test_controller_render_retains_extension_flags_and_selector(self):
        obj = self.controller()
        result = render_controller([obj], dict(self.sessions.lock, object_count=1), 'owner', 'local@sha256:fixed')
        spec = result['items'][0]['spec']
        self.assertEqual(spec['selector'], {'matchLabels': {'app': 'upstream'}})
        container = spec['template']['spec']['containers'][0]
        self.assertEqual(container['args'], ['--leader-elect=true', '--extensions'])
        self.assertEqual(container['imagePullPolicy'], 'Never')
        self.assertEqual(container['image'], 'local@sha256:fixed')
        self.assertEqual(obj['metadata']['labels'][OWNER], 'owner')

    def test_unlocked_controller_or_unexpected_bundle_rejected(self):
        for mutate in ('count', 'image', 'name', 'sidecar'):
            obj = self.controller()
            lock = dict(self.sessions.lock, object_count=1)
            if mutate == 'count': lock['object_count'] = 2
            if mutate == 'image': obj['spec']['template']['spec']['containers'][0]['image'] = 'unlocked:latest'
            if mutate == 'name': obj['metadata']['name'] = 'foreign'
            if mutate == 'sidecar': obj['spec']['template']['spec']['containers'].append({'image': 'foreign'})
            with self.subTest(mutate=mutate), self.assertRaises(SessionError):
                render_controller([obj], lock, 'owner', 'local')

    def test_foreign_resources_never_applied(self):
        path = self.root / 'release.yaml'
        path.write_bytes(b'locked')
        self.sessions.lock['manifest_sha256'] = hashlib.sha256(b'locked').hexdigest()
        self.lab.kubectl.side_effect = [json.dumps(self.controller()), json.dumps({'kind': 'List', 'items': [
            {'metadata': {'labels': {OWNER: 'foreign'}}}]})]
        bundle = {'images': {'controller': {'name': 'local'}}, 'fingerprint': 'f'}
        with patch.object(self.sessions, 'attach'), patch.object(self.sessions, 'bundle', return_value=bundle), \
                patch.object(self.sessions, 'load_images'), \
                patch('lib.platform_sessions.render_controller', return_value={'kind': 'List', 'items': []}):
            with self.assertRaises(SessionError) as caught:
                self.sessions.install(path)
        self.assertEqual(caught.exception.reason, 'unowned_session_resource')
        self.assertFalse(any('apply' in call.args for call in self.lab.kubectl.call_args_list))
        self.lab.write_state.assert_not_called()

    def test_bundle_rejects_changed_inputs_before_image_use(self):
        self.sessions.bundle_path.write_text(json.dumps({'inputs': {'changed': 'hash'}}))
        with self.assertRaises(SessionError) as caught:
            self.sessions.bundle()
        self.assertEqual(caught.exception.reason, 'session_bundle_changed')
        self.lab.docker_info.assert_not_called()

    def test_bundle_rejects_daemon_or_image_replacement(self):
        bundle = {'inputs': {}, 'docker_id': 'owned', 'images': {'client': {'name': 'client', 'id': 'sha256:original'}}}
        self.sessions.bundle_path.write_text(json.dumps(bundle))
        with patch.object(self.sessions, 'build_inputs', return_value={}):
            self.lab.docker_info.return_value = {'ID': 'other'}
            with self.assertRaises(SessionError) as caught:
                self.sessions.bundle()
            self.assertEqual(caught.exception.reason, 'session_bundle_daemon_changed')
            self.lab.docker_info.return_value = {'ID': 'owned'}
            with patch.object(self.sessions, 'docker_image', return_value={'Id': 'sha256:replaced'}):
                with self.assertRaises(SessionError) as caught:
                    self.sessions.bundle()
            self.assertEqual(caught.exception.reason, 'session_image_changed')

    def test_namespace_uid_checked_before_deletion(self):
        self.lab.kubectl.return_value = json.dumps({'metadata': {'uid': 'replaced', 'labels': {OWNER: 'a'*32, RUN: 'run'}}})
        with self.assertRaises(SessionError) as caught:
            self.sessions.close_namespaces({'work': {'name': 'owned', 'uid': 'original'}}, 'run')
        self.assertEqual(caught.exception.reason, 'unowned_session_namespace')
        self.assertFalse(any('delete' in call.args for call in self.lab.kubectl.call_args_list))

    def test_runtime_identity_checked_before_host_probe(self):
        runtime = Mock()
        self.lab.kubectl.return_value = '{"metadata":{"uid":"replaced"}}'
        identity = {'claim': 'claim', 'claim_uid': 'original', 'sandbox': 'sandbox', 'sandbox_uid': 's',
                    'pod': 'pod', 'pod_uid': 'p', 'service': 'service', 'service_uid': 'svc'}
        with self.assertRaises(SessionError) as caught:
            self.sessions.inspect_identity(identity, 'ns', 'run', runtime)
        self.assertEqual(caught.exception.reason, 'session_identity_changed')
        runtime.runtime_evidence.assert_not_called()

    def test_outside_denial_rejects_refusal_or_incomplete_probes(self):
        observer = {'namespace': 'observer', 'name': 'observer', 'uid': 'source'}
        identity = {'pod_uid': 'target', 'pod_ip': '10.244.1.2'}
        for results, reason in [([], 'invalid_observer_response'), ([
                {'id': 'target:8080', 'kind': 'socket_error'}, {'id': 'target:9090', 'kind': 'connect_timeout'}],
                'session_ingress_not_isolated')]:
            self.lab.kubectl.return_value = json.dumps({'pod_uid': 'source', 'run': 'run', 'results': results})
            with patch('lib.platform_sessions.time.monotonic', side_effect=[0, 0, 46]), \
                    patch('lib.platform_sessions.time.sleep'):
                with self.assertRaises(SessionError) as caught:
                    self.sessions.outsider_blocked(observer, [identity], 'run', {})
            self.assertEqual(caught.exception.reason, reason)

    def test_template_uses_managed_custom_policy_and_cold_pool(self):
        obj = self.sessions.render('session-template.json', {'OWNER': 'owner', 'RUN': 'run', 'WORK_NS': 'work',
            'CLIENT_NS': 'client', 'RUNTIME_IMAGE': 'image'})
        template, pool = obj['items']
        spec = template['spec']
        self.assertEqual(pool['spec']['replicas'], 0)
        self.assertEqual(spec['networkPolicyManagement'], 'Managed')
        peer = spec['networkPolicy']['ingress'][0]['from'][0]
        self.assertEqual(peer['namespaceSelector']['matchLabels']['kubernetes.io/metadata.name'], 'client')
        self.assertEqual(peer['podSelector']['matchLabels'][ROLE], 'client')
        self.assertEqual(spec['podTemplate']['spec']['runtimeClassName'], 'agentbox-gvisor')
        self.assertFalse(spec['podTemplate']['spec']['automountServiceAccountToken'])
        self.assertEqual(len(spec['networkPolicy']['egress']), 1)
        self.assertEqual({p['port'] for p in spec['networkPolicy']['egress'][0]['ports']}, {53})

    def test_client_policy_limits_api_and_combines_namespace_with_pod(self):
        self.lab.kubectl.side_effect = [json.dumps({'spec': {'clusterIP': '10.96.0.1'}}), json.dumps({'items': [{
            'endpoints': [{'addresses': ['172.18.0.2']}], 'ports': [{'port': 6443}]}]})]
        spec = self.sessions.client_policy('client', 'work', 'run')['spec']
        self.assertEqual(spec['ingress'], [])
        runtime_peer = spec['egress'][1]['to'][0]
        self.assertEqual(runtime_peer['namespaceSelector']['matchLabels']['kubernetes.io/metadata.name'], 'work')
        self.assertEqual(runtime_peer['podSelector']['matchLabels'][RUN], 'run')
        api_rules = spec['egress'][2:]
        self.assertEqual({r['to'][0]['ipBlock']['cidr'] for r in api_rules}, {'10.96.0.1/32', '172.18.0.2/32'})
        self.assertEqual({p['port'] for r in api_rules for p in r['ports']}, {443, 6443})

    def test_warm_profile_keeps_inventory_and_total_budget_distinct(self):
        template = self.sessions.render('session-template.json', {'OWNER': 'owner', 'RUN': 'run', 'WORK_NS': 'work',
            'CLIENT_NS': 'client', 'RUNTIME_IMAGE': 'image'})
        quota = warm_fixture(template, 'work', 'owner', 'run')
        self.assertEqual(template['items'][1]['metadata']['name'], 'python-warm')
        self.assertEqual(template['items'][1]['spec']['replicas'], 2)
        self.assertEqual(quota['spec']['hard']['pods'], '3')
        self.assertEqual(quota['spec']['hard']['limits.cpu'], '1500m')
        self.assertEqual(quota['spec']['hard']['limits.memory'], '1536Mi')
        original = json.loads((ROOT / 'deploy/platform/session-template.json').read_text())
        self.assertEqual(original['items'][1]['spec']['replicas'], 0)

    def test_invalid_profile_rejected_before_external_operations(self):
        with self.assertRaises(SessionError) as caught:
            self.sessions.test('dynamic')
        self.assertEqual(caught.exception.reason, 'invalid_session_profile')
        self.lab.operation_lock.assert_not_called()
        self.lab.kubectl.assert_not_called()

    def test_warm_supply_replacement_rejected_before_runtime_probe(self):
        self.lab.kubectl.side_effect = [json.dumps({'metadata': {'uid': 'replaced'}}), json.dumps({'metadata': {'uid': 'pod'}})]
        runtime = Mock()
        with self.assertRaises(SessionError) as caught:
            self.sessions.inspect_supply([{'sandbox': 's', 'sandbox_uid': 'original', 'pod': 'p', 'pod_uid': 'pod'}],
                                         'pool', 'work', 'run', runtime, {'runtime_evidence': {}})
        self.assertEqual(caught.exception.reason, 'warm_supply_identity_changed')
        runtime.runtime_evidence.assert_not_called()

    def test_timeout_without_quota_proof_cannot_pass_exhaustion(self):
        with self.assertRaises(SessionError) as caught:
            self.sessions.verify_exhaustion_rollback('work', {}, {})
        self.assertEqual(caught.exception.reason, 'warm_quota_rejection_missing')
        self.lab.kubectl.assert_not_called()

    def test_extra_claim_replay_rejected(self):
        self.lab.kubectl.return_value = json.dumps({'items': [{'metadata': {'uid': 'extra1'}}, {'metadata': {'uid': 'extra2'}}]})
        with self.assertRaises(SessionError) as caught:
            self.sessions.observe_exhaustion('work', 'run', [], {})
        self.assertEqual(caught.exception.reason, 'warm_attempt_replayed')

    def test_failed_claim_identity_replacement_never_counted_as_cleanup(self):
        self.lab.kubectl.side_effect = [json.dumps({'metadata': {'uid': 'q'}, 'spec': {'hard': {'pods': '3'}},
                                                  'status': {'used': {'pods': '3'}}}),
                                        json.dumps({'metadata': {'uid': 'replaced'}})]
        proof = {'claim': {'name': 'claim', 'uid': 'original'}, 'sandbox': {'name': 's', 'uid': 's'}}
        with self.assertRaises(SessionError) as caught:
            self.sessions.verify_exhaustion_rollback('work', {'warm': {'quota_uid': 'q'}}, {'quota_rejection': proof})
        self.assertEqual(caught.exception.reason, 'warm_attempt_identity_changed')

    def test_foreign_pool_never_deleted(self):
        self.lab.kubectl.return_value = json.dumps({'metadata': {'uid': 'foreign', 'labels': {OWNER: 'a'*32}}})
        with self.assertRaises(SessionError) as caught:
            self.sessions.stop_warm_pool('work', {'warm': {'pool_uid': 'owned'}})
        self.assertEqual(caught.exception.reason, 'unowned_warm_pool')
        self.assertFalse(any('delete' in call.args for call in self.lab.kubectl.call_args_list))

    def test_cached_archive_requires_matching_bytes_and_image_ids(self):
        archive = self.root / 'session-images-fingerprint.tar'
        archive.write_bytes(b'archive')
        bundle = {'fingerprint': 'fingerprint', 'docker_id': 'daemon', 'images': {'client': {'name': 'client', 'id': 'sha256:client'}}}
        metadata = dict(docker_id=bundle['docker_id'], images=bundle['images'], sha256=hashlib.sha256(b'archive').hexdigest())
        archive.with_suffix('.json').write_text(json.dumps(metadata))
        self.assertEqual(self.sessions.image_archive(bundle), archive)
        self.lab.runner.run.assert_not_called()
        archive.write_bytes(b'changed')
        with self.assertRaises(SessionError) as caught:
            self.sessions.image_archive(bundle)
        self.assertEqual(caught.exception.reason, 'session_archive_changed')
        archive.write_bytes(b'archive')
        metadata['images']['client']['id'] = 'sha256:changed'
        archive.with_suffix('.json').write_text(json.dumps(metadata))
        bundle['images'] = {'client': {'name': 'client', 'id': 'sha256:client'}}
        with self.assertRaises(SessionError):
            self.sessions.image_archive(bundle)

    def test_failed_export_never_publishes_a_cache(self):
        bundle = {'fingerprint': 'failed', 'docker_id': 'daemon', 'images': {'client': {'name': 'client', 'id': 'id'}}}
        self.lab.runner.run.side_effect = RuntimeError('export failed')
        with self.assertRaises(RuntimeError):
            self.sessions.image_archive(bundle)
        self.assertFalse((self.root / 'session-images-failed.tar').exists())
        self.assertFalse((self.root / 'session-images-failed.json').exists())
        self.lab.write_json.assert_not_called()

    def test_existing_images_require_every_node_tag_and_image_id(self):
        self.sessions.state['node_ids'] = ['node-a', 'node-b']
        bundle = {'images': {'client': {'name': 'agentbox/client:hash', 'id': 'sha256:expected'}}}
        good = json.dumps({'images': [{'repoTags': ['docker.io/agentbox/client:hash'], 'id': 'sha256:expected'}]})
        wrong = json.dumps({'images': [{'repoTags': ['docker.io/agentbox/client:hash'], 'id': 'sha256:other'}]})
        self.lab.runner.run.side_effect = [good, good]
        with patch.object(self.sessions, 'image_archive') as export:
            self.sessions.load_images(bundle)
            export.assert_not_called()
        self.lab.runner.run.side_effect = [good, wrong]
        self.assertFalse(self.sessions.node_images_match(bundle))

    def test_remaining_node_ctr_prevents_overlapping_import(self):
        self.sessions.state['node_ids'] = ['node-a']
        self.lab.runner.run.return_value = 'systemd\nctr\ncontainerd\n'
        with patch.object(self.sessions, 'node_images_match', return_value=False), \
                patch.object(self.sessions, 'image_archive') as export:
            with self.assertRaises(SessionError) as caught:
                self.sessions.load_images({'images': {}})
        self.assertEqual(caught.exception.reason, 'session_import_in_progress')
        export.assert_not_called()

    def test_control_plane_requires_both_native_controllers_ready(self):
        pods = [{'metadata': {'name': role, 'labels': {'component': role}},
                 'status': {'phase': 'Running', 'conditions': [{'type': 'Ready', 'status': 'True'}]}}
                 for role in ('kube-controller-manager', 'kube-scheduler')]
        self.lab.kubectl.return_value = json.dumps({'items': pods})
        self.sessions.control_plane_ready()
        pods[0]['status']['conditions'][0]['status'] = 'False'
        pods[0]['status']['containerStatuses'] = [{'state': {'waiting': {'reason': 'CrashLoopBackOff'}}}]
        self.lab.kubectl.return_value = json.dumps({'items': pods})
        with self.assertRaises(SessionError) as caught:
            self.sessions.control_plane_ready()
        self.assertEqual(caught.exception.reason, 'session_control_plane_not_ready')
        self.assertIn('CrashLoopBackOff', str(caught.exception))
        self.lab.write_state.assert_not_called()

    def test_missing_native_control_plane_member_is_not_ready(self):
        self.lab.kubectl.return_value = '{"items":[]}'
        with self.assertRaises(SessionError) as caught:
            self.sessions.control_plane_ready()
        self.assertEqual(caught.exception.reason, 'session_control_plane_not_ready')

    def test_image_import_waits_for_cri_cache_without_reimport(self):
        self.sessions.state['node_ids'] = ['node-a']
        self.lab.runner.run.return_value = 'systemd\ncontainerd\n'
        with patch.object(self.sessions, 'node_images_match', side_effect=[False, False, True]) as matching, \
                patch.object(self.sessions, 'image_archive', return_value=self.root/'images.tar'), \
                patch('lib.platform_sessions.time.sleep'):
            self.sessions.load_images({'images': {}})
        self.assertEqual(matching.call_count, 3)
        self.assertEqual(sum('image-archive' in call.args[0] for call in self.lab.runner.run.call_args_list), 1)

    def test_image_import_still_rejects_ids_that_never_converge(self):
        self.sessions.state['node_ids'] = ['node-a']
        self.lab.runner.run.return_value = 'systemd\ncontainerd\n'
        with patch.object(self.sessions, 'node_images_match', return_value=False), \
                patch.object(self.sessions, 'image_archive', return_value=self.root/'images.tar'), \
                patch('lib.platform_sessions.time.monotonic', side_effect=[0, 0, 61]), \
                patch('lib.platform_sessions.time.sleep'):
            with self.assertRaises(SessionError) as caught:
                self.sessions.load_images({'images': {}})
        self.assertEqual(caught.exception.reason, 'session_image_load_incomplete')

    def test_final_marker_exit_race_is_not_replayed_or_called_cleanup(self):
        error = RuntimeError('exec killed as client exits')
        error.reason = 'command_failed'
        self.lab.kubectl.side_effect = error
        self.lab.runner.commands = [{'exit_code':137}]
        record = {'cleanup_complete':False}
        self.sessions.send_marker('client', 'pod', 'warm-finish', record)
        self.lab.kubectl.assert_called_once()
        self.assertFalse(record['cleanup_complete'])
        self.assertEqual(record['final_marker_transport_error']['exit_code'],137)

    def test_other_marker_errors_remain_fatal(self):
        error = RuntimeError('exec failed')
        error.reason = 'command_failed'
        self.lab.kubectl.side_effect = error
        for marker,code in [('warm-finish',1),('warm-start',137),('warm-release',137)]:
            self.lab.runner.commands = [{'exit_code':code}]
            with self.subTest(marker=marker,code=code), self.assertRaises(RuntimeError):
                self.sessions.send_marker('client','pod',marker,{})


if __name__ == '__main__':
    unittest.main()
