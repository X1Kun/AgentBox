"""Network provider ownership and policy acceptance regressions; no cluster required."""
from contextlib import nullcontext, redirect_stdout
import hashlib
import io
import json
from pathlib import Path
import socket
import shutil
import struct
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.platform_network import Network, NetworkError, checked_manifest, image_sources, kubectl_objects, observation_matches, render_calico
from lib import network_client
import platform_lab


class NetworkTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.lab = Mock()
        self.lab.config = ROOT / 'deploy/platform'
        self.lab.root = ROOT
        self.lab.runner.output = self.root
        self.lab.operation_lock.return_value = nullcontext()
        self.net = Network(self.lab)
        self.net.state = {'session': 'a' * 32}

    def test_manifest_checksum_is_checked_before_any_cluster_operation(self):
        path = self.root / 'calico.yaml'
        path.write_text('wrong manifest')
        with self.assertRaises(NetworkError) as caught:
            self.net.install(path)
        self.assertEqual(caught.exception.reason, 'calico_checksum_mismatch')
        self.lab.kubectl.assert_not_called()
        self.lab.owned.assert_not_called()

    def test_kubectl_json_stream_and_list_are_both_supported(self):
        a, b = {'kind': 'A', 'metadata': {}}, {'kind': 'B', 'metadata': {}}
        self.assertEqual(kubectl_objects(json.dumps(a)+'\n'+json.dumps(b)), [a,b])
        self.assertEqual(kubectl_objects(json.dumps({'kind':'List','items':[a,b]})), [a,b])
        self.assertEqual(kubectl_objects(' \n'), [])
        with self.assertRaises(ValueError):
            kubectl_objects(json.dumps(a)+' broken')

    def test_mirror_preserves_every_locked_digest(self):
        mirror = image_sources(self.net.lock, 'quay.mirror.example/calico')
        for name, image in mirror.items():
            self.assertTrue(image.startswith('quay.mirror.example/calico/'))
            self.assertEqual(image.split('@')[1], self.net.lock['images'][name].split('@')[1])
        for prefix in ('https://mirror.example/calico', 'mirror/calico@sha256:abc', 'mirror/../calico?x'):
            with self.subTest(prefix=prefix), self.assertRaises(NetworkError):
                image_sources(self.net.lock, prefix)

    def test_render_pins_init_and_main_images_without_widening_selectors(self):
        original = next(iter(self.net.lock['images']))
        lock = dict(self.net.lock, object_count=2, images={original:self.net.lock['images'][original]})
        objects = [
            {'kind':'ConfigMap','metadata':{'name':'calico-config'},'data':{'calico_backend':'vxlan'}},
            {'kind':'DaemonSet','metadata':{'name':'calico-node'},'spec':{'selector':{'matchLabels':{'k8s-app':'calico-node'}},
                'template':{'metadata':{'labels':{'k8s-app':'calico-node'}},'spec':{
                    'initContainers':[{'name':'install-cni','image':original}],
                    'containers':[{'name':'calico-node','image':original,'env':[]}]
                }}}}]
        result = render_calico(objects, lock, 'session')
        spec = result['items'][1]['spec']
        self.assertEqual(spec['selector'], {'matchLabels':{'k8s-app':'calico-node'}})
        self.assertEqual(spec['template']['spec']['initContainers'][0]['image'], lock['images'][original])
        env = {e['name']:e.get('value') for e in spec['template']['spec']['containers'][0]['env']}
        self.assertEqual(env['CALICO_IPV4POOL_CIDR'], '10.244.0.0/16')
        self.assertEqual(env['CALICO_IPV4POOL_VXLAN'], 'Always')
        self.assertEqual(env['CALICO_IPV4POOL_IPIP'], 'Never')
        self.assertEqual(env['FELIX_BPFENABLED'], 'false')

    def test_render_rejects_unexpected_manifest_or_image(self):
        with self.assertRaises(NetworkError):
            render_calico([], self.net.lock, 'session')
        obj = {'kind':'Deployment','metadata':{'name':'other'},'spec':{'template':{'spec':{
            'containers':[{'name':'other','image':'unlocked:latest'}]}}}}
        with self.assertRaises(NetworkError) as caught:
            render_calico([obj], dict(self.net.lock, object_count=1), 'session')
        self.assertEqual(caught.exception.reason, 'unexpected_calico_image')

    def test_existing_kindnet_profile_is_never_replaced(self):
        self.lab.owned.return_value = ({'network_profile':'kindnet'}, [])
        with self.assertRaises(NetworkError) as caught:
            self.net.attach()
        self.assertEqual(caught.exception.reason, 'wrong_network_profile')
        self.lab.kubectl.assert_not_called()
        self.lab.write_state.assert_not_called()

    def test_install_refuses_foreign_calico_resources_before_apply(self):
        self.lab.kubectl.side_effect = ['', '{"kind":"List","items":[]}', json.dumps({
            'kind':'List','items':[{'metadata':{'name':'calico-node','labels':{'agentbox.dev/session':'foreign'}}}]})]
        with patch.object(self.net, 'attach'), patch('lib.platform_network.checked_manifest', return_value=b''), \
                patch('lib.platform_network.render_calico', return_value={'kind':'List','items':[]}):
            with self.assertRaises(NetworkError) as caught:
                self.net.install(self.root/'unused')
        self.assertEqual(caught.exception.reason, 'unowned_calico_resource')
        self.lab.write_state.assert_not_called()
        self.assertFalse(any('apply' in call.args for call in self.lab.kubectl.call_args_list))

    def test_denial_requires_connection_timeout_not_refusal_or_bad_reply(self):
        case = {'protocol':'http'}
        for kind in ('socket_error','read_timeout','invalid_reply','allowed','unexpected_timeout'):
            self.assertFalse(observation_matches(case, {'kind':kind}, False, 'run'))
        self.assertTrue(observation_matches(case, {'kind':'connect_timeout'}, False, 'run'))

    def test_allowed_http_requires_expected_pod_run_and_port(self):
        case = {'protocol':'http','target_uid':'uid','port':8000}
        good = {'kind':'allowed','response':{'pod_uid':'uid','run':'run','port':8000}}
        self.assertTrue(observation_matches(case, good, True, 'run'))
        for key, value in [('pod_uid','other'),('run','old'),('port',8001)]:
            changed=json.loads(json.dumps(good)); changed['response'][key]=value
            self.assertFalse(observation_matches(case, changed, True, 'run'))

    def test_dns_requires_expected_service_ip(self):
        case = {'protocol':'dns-udp','expected_ip':'10.96.0.1'}
        self.assertTrue(observation_matches(case, {'kind':'allowed','addresses':['10.96.0.1']}, True, 'run'))
        self.assertFalse(observation_matches(case, {'kind':'allowed','addresses':['1.1.1.1']}, True, 'run'))
        self.assertTrue(observation_matches(case, {'kind':'dns_timeout'}, False, 'run'))
        self.assertFalse(observation_matches(case, {'kind':'invalid_reply'}, False, 'run'))

    def test_convergence_requires_two_consecutive_complete_matches(self):
        record={'phases':{}}
        values=[[{'matches':True}],[{'matches':False}],[{'matches':True}],[{'matches':True}]]
        with patch.object(self.net, 'observe', side_effect=values) as observe, patch.object(self.net,'save'), \
                patch('lib.platform_network.time.sleep'):
            self.net.converge({}, [], 'allow', 'run', record)
        self.assertEqual(observe.call_count, 4)

    def test_namespace_cleanup_refuses_replaced_uid(self):
        self.lab.kubectl.return_value=json.dumps({'metadata':{'uid':'replaced','labels':{}}})
        with self.assertRaises(NetworkError):
            self.net.cleanup_namespaces({'sandbox':{'name':'sandbox','uid':'original'}}, 'run')
        self.assertEqual(self.lab.kubectl.call_count, 1)

    def test_policy_matrix_covers_both_runtimes_with_same_cases(self):
        keys=('sandbox-runc','sandbox-gvisor','caller','intruder','foreign-caller','approved','blocked','foreign-data')
        pods={k:{'name':k,'uid':k+'-uid','ip':'10.244.1.2'} for k in keys}
        cases=self.net.matrix(pods,'10.96.0.1')
        self.assertEqual(len(cases),22)
        for runtime in ('sandbox-runc','sandbox-gvisor'):
            selected=[c for c in cases if c['id'].startswith(runtime+':')]
            self.assertEqual(sum(c['allowed'] for c in selected),4)
            self.assertEqual({c['id'].split(':')[1] for c in selected}, {'control','wrong-ingress-port',
                'wrong-source-label','wrong-source-namespace','data','wrong-egress-port','wrong-target-label',
                'wrong-target-namespace','api','dns-udp','dns-tcp'})

    def test_probe_distinguishes_connect_timeout_read_timeout_and_refusal(self):
        case={'id':'case','protocol':'http','ip':'10.0.0.1','port':80}
        for connect_error, recv_error, expected in [(socket.timeout(),None,'connect_timeout'),
                                                  (None,socket.timeout(),'read_timeout'),
                                                  (ConnectionRefusedError(111,'refused'),None,'socket_error')]:
            sock=Mock(); sock.connect.side_effect=connect_error; sock.recv.side_effect=recv_error
            with patch('lib.network_client.socket.socket') as factory:
                factory.return_value.__enter__.return_value=sock
                self.assertEqual(network_client.run([case])[0]['kind'],expected)

    def test_dns_parser_handles_compressed_answers_and_rejects_wrong_id(self):
        question=b'\x0akubernetes\x07default\x03svc\x07cluster\x05local\x00'+struct.pack('!HH',1,1)
        response=struct.pack('!HHHHHH',123,0x8180,1,1,0,0)+question+b'\xc0\x0c'+struct.pack('!HHIH',1,1,30,4)+socket.inet_aton('10.96.0.1')
        self.assertEqual(network_client.dns_addresses(response,123), ['10.96.0.1'])
        with self.assertRaises(ValueError): network_client.dns_addresses(response,124)

    def calico_create_fixture(self):
        config=self.root/'deploy/platform'
        config.mkdir(parents=True)
        for name in ('versions.lock.json','kind.yaml','kind-calico.yaml','calico.lock.json'):
            shutil.copyfile(ROOT/'deploy/platform'/name,config/name)
        runner=Mock(); runner.output=self.root/'evidence'; runner.output.mkdir()
        lab=platform_lab.Lab(self.root,runner)
        def run(args,**kwargs):
            args=[str(a) for a in args]
            if 'create' in args:
                lab.kubeconfig.write_text('dedicated config')
            if 'version' in args:
                return json.dumps({'serverVersion':{'gitVersion':lab.lock['kubernetes']['version']}})
            return ''
        runner.run.side_effect=run
        with patch.object(lab,'preflight',return_value={'ok':True,'docker':{'ID':'docker'}}), \
                patch.object(lab,'prepare_image'), patch.object(lab,'node_ids',side_effect=[[],['1'*64,'2'*64]]), \
                patch.object(lab,'node_report',return_value=[]) as report:
            result=lab.create(network_profile='calico')
        report.assert_called_once_with(require_ready=False)
        return lab,runner,result['state']

    def test_custom_cni_bootstrap_stays_pending_and_cannot_run_jobs(self):
        lab,runner,state=self.calico_create_fixture()
        self.assertEqual(state['phase'],'network_pending')
        self.assertEqual(state['network_profile'],'calico')
        calls=[list(map(str,c.args[0])) for c in runner.run.call_args_list]
        create=next(c for c in calls if 'create' in c)
        self.assertEqual(create[create.index('--wait')+1],'0s')
        self.assertFalse(any('wait' in c for c in calls))
        with patch.object(lab,'docker_info',return_value={'ID':'docker'}), patch.object(lab,'node_ids',return_value=state['node_ids']):
            with self.assertRaises(platform_lab.LabError) as caught:
                lab.owned(complete=True)
        self.assertEqual(caught.exception.reason,'cluster_not_ready')

    def test_changed_kind_config_blocks_pending_network_install(self):
        lab,_,state=self.calico_create_fixture()
        path=lab.config/'kind-calico.yaml'; path.write_text(path.read_text()+'# changed\n')
        with patch.object(lab,'docker_info',return_value={'ID':'docker'}), patch.object(lab,'node_ids',return_value=state['node_ids']):
            with self.assertRaises(NetworkError) as caught:
                Network(lab).attach()
        self.assertEqual(caught.exception.reason,'network_definition_changed')

    def test_owned_cluster_can_be_deleted_after_kind_config_changes(self):
        lab,_,state=self.calico_create_fixture()
        path=lab.config/'kind-calico.yaml'; path.write_text(path.read_text()+'# desired config changed\n')
        with patch.object(lab,'docker_info',return_value={'ID':'docker'}), \
                patch.object(lab,'node_ids',side_effect=[state['node_ids'],[]]):
            result=lab.delete()
        self.assertTrue(result['cleanup_complete'])
        self.assertEqual(lab.read_state()['phase'],'deleted')

    def test_network_dry_runs_do_not_execute_or_write(self):
        for action in ('create','network-install','network-test'):
            path=self.root/action
            args=[action,'--dry-run','--output',str(path)]
            if action=='create': args+=['--network-profile','calico']
            with patch.object(platform_lab.Runner,'run',side_effect=AssertionError('executed')), redirect_stdout(io.StringIO()):
                self.assertEqual(platform_lab.main(args),0)
            self.assertFalse(path.exists())

    def test_image_preparation_reuses_cache_and_verifies_exact_source_digest(self):
        images = image_sources(self.net.lock, 'domestic.example/calico')
        expected = list(sorted(set(images.values())))
        self.net.state['node_ids'] = ['owned-node']
        self.lab.tools = {'docker': 'docker'}
        inventory = [{'id': 'first', 'repoDigests': [expected[0]]}]
        pulls = []
        def run(args, **kwargs):
            if 'pull' in args:
                pulls.append(args[-1]);inventory.append({'id': 'new', 'repoDigests': [args[-1]]});return ''
            return json.dumps({'images': inventory})
        self.lab.runner.run.side_effect = run
        result = self.net.prepare_images(images)
        self.assertEqual(pulls, expected[1:])
        self.assertEqual([v['cached'] for v in result], [True, False, False])
        self.assertEqual({v['source'] for v in result}, set(expected))
        self.assertTrue(all(call.args[0][2] == 'owned-node' for call in self.lab.runner.run.call_args_list))

    def test_image_preparation_rejects_unconfirmed_pull_without_applying_manifest(self):
        self.net.state['node_ids'] = ['owned-node'];self.lab.tools = {'docker': 'docker'}
        self.lab.runner.run.return_value = '{"images":[]}'
        with patch('lib.platform_network.time.monotonic', side_effect=[0, 0, 31]), self.assertRaises(NetworkError) as caught:
            self.net.prepare_images(image_sources(self.net.lock, None))
        self.assertEqual(caught.exception.reason, 'calico_image_not_ready')
        self.lab.kubectl.assert_not_called()


if __name__=='__main__':
    unittest.main()
