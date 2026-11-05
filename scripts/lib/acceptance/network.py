"""NetworkPolicy acceptance using Pod-local probes in owned namespaces."""

from lib.core.paths import ROOT as PROJECT_ROOT
from lib.manifests import render as render_manifest, render_upstream
import ipaddress
import json
from pathlib import Path
import re
import time
import uuid

from lib.platform.gvisor import GVisor

OWNER = 'agentbox.dev/session'
ROLE = 'agentbox.dev/net-role'


from lib.platform.calico import Network
from lib.core.errors import NetworkError

class NetworkAcceptance:
    def __init__(self, lab, platform=None):
        self.platform = platform or Network(lab)
        self.lab = lab
        self.output = lab.runner.output

    def namespace(self, name, role, run_id):
        obj = render_manifest(self.lab.config, 'network-namespace', {'NAME': name, 'ROLE': role, 'RUN': run_id, 'OWNER': self.platform.state['session']})
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        return {'name': name, 'uid': created['metadata']['uid']}


    def pod(self, name, namespace, role, run_id, image, worker=False, runtime=None):
        obj = render_manifest(self.lab.config, 'network-worker' if worker else 'network-control',
            {'NAME': name, 'NAMESPACE': namespace, 'ROLE': role, 'RUN': run_id, 'OWNER': self.platform.state['session'], 'IMAGE': image})
        spec = obj['spec']
        node = 'agentbox-platform-lab-' + ('worker' if worker else 'control-plane')
        if runtime:
            spec['runtimeClassName'] = runtime
        spec['containers'][0]['command'] = ['/usr/local/bin/python3', '-I', '-B', '-u', '-c',
                                           (self.lab.root / 'tests/fixtures/probes/network_server.py').read_text()]
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        return {'name': name, 'namespace': namespace, 'uid': created['metadata']['uid'],
                'role': role, 'run': run_id, 'image': image, 'node': node, 'runtime': runtime}


    def live_pod(self, expected):
        obj = json.loads(self.lab.kubectl('-n', expected['namespace'], 'get', 'pod', expected['name'],
                                          '-o', 'json', announce=False))
        if (obj['metadata']['uid'] != expected['uid'] or
                obj['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session'] or
                obj['metadata'].get('labels', {}).get('agentbox.dev/net-run') != expected['run'] or
                obj['spec'].get('nodeName') != expected['node'] or
                obj['spec'].get('runtimeClassName') != expected['runtime'] or
                len(obj['spec'].get('containers', [])) != 1 or
                obj['spec']['containers'][0]['image'] != expected['image'] or
                obj['status'].get('phase') != 'Running' or
                not any(c['type'] == 'Ready' and c['status'] == 'True' for c in obj['status'].get('conditions', [])) or
                any(c.get('restartCount', 0) for c in obj['status'].get('containerStatuses', []))):
            raise NetworkError('network_probe_identity_changed', expected['name'])
        ip = obj['status']['podIP']
        if ipaddress.ip_address(ip) not in ipaddress.ip_network(self.platform.lock['pod_cidr']):
            raise NetworkError('unexpected_pod_network', ip)
        expected['ip'] = ip
        return obj


    def policies(self, namespace, run_id):
        return render_manifest(self.lab.config, 'network-policy', {'NAMESPACE': namespace, 'RUN': run_id, 'OWNER': self.platform.state['session']})['items']


    def apply_policies(self, policies):
        value = {'apiVersion': 'v1', 'kind': 'List', 'items': policies}
        self.lab.kubectl('create', '-f', '-', input_text=json.dumps(value))


    def delete_policies(self, namespace, names, run_id):
        for name in names:
            value = json.loads(self.lab.kubectl('-n', namespace, 'get', 'networkpolicy', name, '-o', 'json'))
            if (value['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session'] or
                    value['metadata']['labels'].get('agentbox.dev/net-run') != run_id):
                raise NetworkError('unowned_networkpolicy', name)
            self.lab.kubectl('-n', namespace, 'delete', 'networkpolicy', name, '--wait=true')


    def matrix(self, pods, api_ip):
        cases = []

        def http(case_id, source, target, port, allowed):
            cases.append({'id': case_id, 'source': source['name'], 'protocol': 'http',
                          'ip': target['ip'], 'port': port, 'target_uid': target['uid'], 'allowed': allowed})

        for sandbox in (pods['sandbox-runc'], pods['sandbox-gvisor']):
            prefix = sandbox['name'] + ':'
            http(prefix + 'control', pods['caller'], sandbox, 8000, True)
            http(prefix + 'wrong-ingress-port', pods['caller'], sandbox, 8001, False)
            http(prefix + 'wrong-source-label', pods['intruder'], sandbox, 8000, False)
            http(prefix + 'wrong-source-namespace', pods['foreign-caller'], sandbox, 8000, False)
            http(prefix + 'data', sandbox, pods['approved'], 8080, True)
            http(prefix + 'wrong-egress-port', sandbox, pods['approved'], 8081, False)
            http(prefix + 'wrong-target-label', sandbox, pods['blocked'], 8080, False)
            http(prefix + 'wrong-target-namespace', sandbox, pods['foreign-data'], 8080, False)
            cases.append({'id': prefix + 'api', 'source': sandbox['name'], 'protocol': 'tcp',
                          'ip': api_ip, 'port': 443, 'allowed': False})
            for protocol in ('dns-udp', 'dns-tcp'):
                cases.append({'id': prefix + protocol, 'source': sandbox['name'], 'protocol': protocol,
                              'name': 'kubernetes.default.svc.cluster.local', 'expected_ip': api_ip, 'allowed': True})
        return cases


    def observe(self, pods, cases, phase, run_id):
        results = []
        program = (self.lab.root / 'tests/fixtures/probes/network_client.py').read_text()
        for source_name in sorted({c['source'] for c in cases}):
            source = pods[source_name]
            self.live_pod(source)
            selected = [c for c in cases if c['source'] == source_name]
            output = json.loads(self.lab.kubectl('-n', source['namespace'], 'exec', source['name'], '-c', 'compute',
                                  '--', '/usr/local/bin/python3', '-I', '-B', '-c', program, json.dumps(selected),
                                  timeout=45, announce=False))
            if output['pod_uid'] != source['uid'] or output['run'] != run_id:
                raise NetworkError('network_probe_identity_changed', source_name)
            if len(output['results']) != len(selected) or {r['id'] for r in output['results']} != {c['id'] for c in selected}:
                raise NetworkError('incomplete_network_observation', source_name)
            for case, value in zip(selected, output['results']):
                if value['id'] != case['id']:
                    raise NetworkError('invalid_network_observation', 'case ordering changed')
                expected = True if phase in ('baseline', 'restored') else case['allowed'] if phase == 'allow' else False
                valid = observation_matches(case, value, expected, run_id)
                results.append({'case': case['id'], 'source_uid': source['uid'], 'expected_allowed': expected,
                                'observation': value, 'matches': valid})
        return results


    def converge(self, pods, cases, phase, run_id, record):
        until = time.monotonic() + 90
        consecutive = 0
        samples = []
        record['phases'][phase] = samples
        while time.monotonic() < until:
            results = self.observe(pods, cases, phase, run_id)
            samples.append({'observed_at': time.time(), 'results': results})
            self.platform.save('network-test.json', record)
            consecutive = consecutive + 1 if all(r['matches'] for r in results) else 0
            if consecutive >= 2:
                # Targets must still be alive; socket refusal/closed listeners never count as denial.
                for pod in pods.values():
                    self.live_pod(pod)
                return
            time.sleep(.5)
        raise NetworkError('network_policy_not_converged', phase + ': ' + json.dumps(samples[-1])[:4000])


    def cleanup_namespaces(self, namespaces, run_id):
        for namespace in namespaces.values():
            obj = json.loads(self.lab.kubectl('get', 'namespace', namespace['name'], '-o', 'json'))
            if (obj['metadata']['uid'] != namespace['uid'] or obj['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session'] or
                    obj['metadata']['labels'].get('agentbox.dev/net-run') != run_id):
                raise NetworkError('unowned_network_namespace', namespace['name'])
            self.lab.kubectl('delete', 'namespace', namespace['name'], '--wait=true', '--timeout=90s', timeout=110)
        for namespace in namespaces.values():
            if self.lab.kubectl('get', 'namespace', namespace['name'], '--ignore-not-found', '-o', 'json').strip():
                raise NetworkError('network_cleanup_incomplete', namespace['name'])


    def test(self, image):
        with self.lab.operation_lock():
            self.platform.attach(ready=True)
            self.platform.verify()
            runtime = GVisor(self.lab)
            runtime.attach()
            runtime.verify_installation()
            run_id = uuid.uuid4().hex[:10]
            record = {'run': run_id, 'session': self.platform.state['session'], 'phases': {}, 'cleanup_complete': False}
            namespaces = {}
            pods = {}
            record.update(namespaces=namespaces, pods=pods)
            try:
                for role in ('sandbox', 'control', 'data', 'foreign'):
                    namespaces[role] = self.namespace('agentbox-net-' + run_id + '-' + role, role, run_id)
                definitions = [('sandbox-runc', 'sandbox', 'sandbox', True, None),
                               ('sandbox-gvisor', 'sandbox', 'sandbox', True, 'agentbox-gvisor'),
                               ('caller', 'control', 'caller', False, None),
                               ('intruder', 'control', 'intruder', False, None),
                               ('foreign-caller', 'foreign', 'caller', False, None),
                               ('approved', 'data', 'approved', False, None),
                               ('blocked', 'data', 'blocked', False, None),
                               ('foreign-data', 'foreign', 'approved', False, None)]
                for name, ns, role, worker, handler in definitions:
                    pods[name] = self.pod(name, namespaces[ns]['name'], role, run_id, image, worker, handler)
                record.update(namespaces=namespaces, pods=pods)
                self.platform.save('network-test.json', record)
                for namespace in namespaces.values():
                    self.lab.kubectl('-n', namespace['name'], 'wait', '--for=condition=Ready', 'pods', '--all', '--timeout=240s', timeout=260)
                for pod in pods.values():
                    self.live_pod(pod)
                record['gvisor_runtime'] = runtime.runtime_evidence(self.live_pod(pods['sandbox-gvisor']))
                api = json.loads(self.lab.kubectl('-n', 'default', 'get', 'service', 'kubernetes', '-o', 'json'))['spec']['clusterIP']
                cases = self.matrix(pods, api)
                record['cases'] = cases
                policies = self.policies(namespaces['sandbox']['name'], run_id)
                self.platform.save('network-policies.json', policies)
                print('[platform] network phase: baseline (all listeners reachable)', flush=True)
                self.converge(pods, cases, 'baseline', run_id, record)
                deny = [p for p in policies if p['metadata']['name'] == 'default-deny']
                allow = [p for p in policies if p['metadata']['name'] != 'default-deny']
                if len(deny) != 1: raise NetworkError('invalid_network_policy_set', 'expected exactly one default deny policy')
                self.apply_policies(deny)
                print('[platform] network phase: default deny', flush=True)
                self.converge(pods, cases, 'deny', run_id, record)
                self.apply_policies(allow)
                print('[platform] network phase: selected control/data/DNS allowed', flush=True)
                self.converge(pods, cases, 'allow', run_id, record)
                self.delete_policies(namespaces['sandbox']['name'], [p['metadata']['name'] for p in allow], run_id)
                print('[platform] network phase: revoke allow rules', flush=True)
                self.converge(pods, cases, 'revoked', run_id, record)
                self.delete_policies(namespaces['sandbox']['name'], ['default-deny'], run_id)
                print('[platform] network phase: restore baseline', flush=True)
                self.converge(pods, cases, 'restored', run_id, record)
                self.cleanup_namespaces(namespaces, run_id)
                if runtime.processes(record['gvisor_runtime']['sandbox_id']):
                    raise NetworkError('network_cleanup_incomplete', 'gVisor processes remain after namespace deletion')
                record.update(cleanup_complete=True, all_passed=True)
                return record
            finally:
                self.platform.save('network-test.json', record)


def observation_matches(case, value, expected, run_id):
    if not expected:
        return value['kind'] == ('dns_timeout' if case['protocol'].startswith('dns-') else 'connect_timeout')
    if value['kind'] != 'allowed':
        return False
    if case['protocol'] == 'http':
        return value.get('response') == {'pod_uid': case['target_uid'], 'run': run_id, 'port': case['port']}
    if case['protocol'].startswith('dns-'):
        return case['expected_ip'] in value.get('addresses', [])
    return True

