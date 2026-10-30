"""Calico bootstrap and NetworkPolicy probes for a fresh, owned kind lab."""
import ipaddress
import json
from pathlib import Path
import re
import time
import uuid

from lib.platform_gvisor import GVisor, digest

OWNER = 'agentbox.dev/session'
ROLE = 'agentbox.dev/net-role'


class NetworkError(Exception):
    def __init__(self, reason, detail):
        super().__init__(detail)
        self.reason = reason


def checked_manifest(path, lock):
    data = Path(path).read_bytes()
    import hashlib
    if hashlib.sha256(data).hexdigest() != lock['sha256']:
        raise NetworkError('calico_checksum_mismatch', 'manifest differs from the pinned upstream content')
    return data


def kubectl_objects(text):
    # kubectl create -o json emits one JSON document per input YAML document.
    decoder = json.JSONDecoder()
    result = []
    remaining = text.lstrip()
    while remaining:
        value, end = decoder.raw_decode(remaining)
        result.extend(value['items'] if value.get('kind') == 'List' else [value])
        remaining = remaining[end:].lstrip()
    return result


def image_sources(lock, prefix=None):
    if prefix and not re.fullmatch(r'[A-Za-z0-9._:-]+(?:/[A-Za-z0-9._-]+)*', prefix):
        raise NetworkError('invalid_image_prefix', 'use a registry/repository prefix without URL scheme or digest')
    result = {}
    for original, canonical in lock['images'].items():
        if not re.fullmatch(r'[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}', canonical):
            raise NetworkError('invalid_calico_lock', 'all images must be digest pinned')
        result[original] = (prefix.rstrip('/') + '/' + canonical.split('/')[-1]) if prefix else canonical
    return result


def render_calico(objects, lock, session, prefix=None):
    if len(objects) != lock['object_count']:
        raise NetworkError('unexpected_calico_manifest', 'upstream object count changed')
    sources = image_sources(lock, prefix)
    seen = set()
    for obj in objects:
        obj['metadata'].setdefault('labels', {}).update({OWNER: session, 'app.kubernetes.io/managed-by': 'agentbox-platform'})
        if obj['kind'] == 'ConfigMap' and obj['metadata']['name'] == 'calico-config':
            if obj['data']['calico_backend'] != 'vxlan':
                raise NetworkError('unexpected_calico_backend', 'use the official VXLAN manifest')
        if obj['kind'] not in ('DaemonSet', 'Deployment'):
            continue
        spec = obj['spec']['template']['spec']
        for container in spec.get('initContainers', []) + spec['containers']:
            original = container['image']
            if original not in sources:
                raise NetworkError('unexpected_calico_image', original)
            container['image'] = sources[original]
            seen.add(original)
            if container['name'] == 'calico-node':
                env = {e['name']: e for e in container['env']}
                for key, value in {'CALICO_IPV4POOL_CIDR': lock['pod_cidr'], 'CALICO_IPV4POOL_IPIP': 'Never',
                                   'CALICO_IPV4POOL_VXLAN': 'Always', 'FELIX_IPV6SUPPORT': 'false',
                                   'FELIX_BPFENABLED': 'false', 'CLUSTER_TYPE': 'k8s'}.items():
                    env[key] = {'name': key, 'value': value}
                container['env'] = list(env.values())
    if seen != set(sources):
        raise NetworkError('unexpected_calico_images', 'not all locked image components were present')
    return {'apiVersion': 'v1', 'kind': 'List', 'items': objects}


class Network:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.lock = json.loads((lab.config / 'calico.lock.json').read_text())
        if (self.lock.get('schema_version') != 1 or self.lock.get('mode') != 'VXLAN' or
                not re.fullmatch('[a-f0-9]{64}', self.lock.get('sha256', ''))):
            raise NetworkError('invalid_calico_lock', 'unsupported Calico definition')
        self.state = None

    def definition(self):
        return {p: digest(self.lab.config / p) for p in ('calico.lock.json', 'kind-calico.yaml')}

    def attach(self, ready=False):
        self.state, actual = self.lab.owned(complete=ready)
        if (self.state.get('network_profile') != 'calico' or len(actual) != 2 or
                actual != self.state.get('node_ids') or self.state['phase'] not in ('network_pending', 'ready')):
            raise NetworkError('wrong_network_profile', 'requires a fresh lab created with --network-profile calico')
        if self.state.get('network_definition') != self.definition():
            raise NetworkError('network_definition_changed', 'recreate the lab before changing the CNI definition')
        if digest(self.lab.config / 'versions.lock.json') != self.state['lock_sha256']:
            raise NetworkError('baseline_changed', 'platform version lock changed')
        self.lab.node_report(require_ready=ready)

    def save(self, name, value):
        (self.output / name).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')

    def verify(self):
        installed = self.state.get('network', {})
        if installed.get('phase') != 'ready' or installed.get('definition') != self.definition():
            raise NetworkError('network_not_ready', 'complete the pinned Calico installation first')
        for resource, name, uid in installed['components']:
            obj = json.loads(self.lab.kubectl('-n', 'kube-system', 'get', resource, name, '-o', 'json'))
            if obj['metadata']['uid'] != uid or obj['metadata'].get('labels', {}).get(OWNER) != self.state['session']:
                raise NetworkError('calico_identity_changed', name)
            status = obj.get('status', {})
            if (status.get('observedGeneration', 0) < obj['metadata'].get('generation', 1) or
                    (resource == 'daemonset' and (status.get('desiredNumberScheduled') != 2 or status.get('numberReady') != 2)) or
                    (resource == 'deployment' and status.get('availableReplicas', 0) < 1)):
                raise NetworkError('calico_not_ready', name)
            spec = obj['spec']['template']['spec']
            images = [c['image'] for c in spec.get('initContainers', []) + spec['containers']]
            if not set(images).issubset(set(installed['images'].values())):
                raise NetworkError('calico_image_changed', name)
        pool = json.loads(self.lab.kubectl('get', 'ippools.crd.projectcalico.org', '-o', 'json'))['items']
        if len(pool) != 1 or (pool[0]['spec'].get('cidr'), pool[0]['spec'].get('vxlanMode'), pool[0]['spec'].get('ipipMode')) != (self.lock['pod_cidr'], 'Always', 'Never'):
            raise NetworkError('calico_pool_changed', 'expected the locked IPv4 VXLAN pool')
        return {'pool': pool[0]['spec'], 'images': installed['images'], 'components': installed['components']}

    def prepare_images(self, images):
        """Separate node image preparation from the existing Pod rollout budget."""
        records = []
        for node in self.state['node_ids']:
            def cached():
                return json.loads(self.lab.runner.run([self.lab.tools['docker'], 'exec', node,
                    'crictl', 'images', '-o', 'json']))['images']
            inventory = cached()
            for image in sorted(set(images.values())):
                started = time.monotonic()
                found = [v for v in inventory if image in v.get('repoDigests', [])]
                reused = bool(found)
                if not found:
                    self.lab.runner.run([self.lab.tools['docker'], 'exec', node, 'crictl', 'pull', image], timeout=180)
                    # Pull completion can precede the CRI image cache update.
                    until = time.monotonic() + 30
                    while time.monotonic() < until:
                        inventory = cached()
                        found = [v for v in inventory if image in v.get('repoDigests', [])]
                        if found:
                            break
                        time.sleep(.5)
                if len(found) != 1:
                    raise NetworkError('calico_image_not_ready', 'CRI did not confirm the pinned image digest on the owned node')
                records.append({'node': node, 'source': image, 'image_id': found[0]['id'],
                    'cached': reused, 'elapsed_seconds': round(time.monotonic()-started, 3)})
                self.save('network-image-preparation.json', {'images': records})
        return records

    def install(self, source, prefix=None):
        data = checked_manifest(source, self.lock)
        with self.lab.operation_lock():
            self.attach()
            images = image_sources(self.lock, prefix)
            previous = self.state.get('network')
            if previous and (previous['definition'] != self.definition() or previous['images'] != images):
                raise NetworkError('network_definition_changed', 'recreate instead of replacing a live CNI or image source')
            if previous and previous.get('phase') == 'ready':
                result = self.verify()
                result['already_installed'] = True
                return result
            if self.lab.kubectl('-n', 'kube-system', 'get', 'daemonset', 'kindnet', '--ignore-not-found', '-o', 'json').strip():
                raise NetworkError('existing_cni', 'never install Calico over kindnet')
            upstream = self.output / 'calico-upstream.yaml'
            upstream.write_bytes(data)
            # Delegate YAML parsing to the pinned kubectl, without requiring PyYAML.
            parsed = kubectl_objects(self.lab.kubectl('create', '--dry-run=client', '--validate=false', '-f', upstream, '-o', 'json', timeout=60))
            rendered = render_calico(parsed, self.lock, self.state['session'], prefix)
            self.save('calico-rendered.json', rendered)
            found_text = self.lab.kubectl('get', '-f', '-', '--ignore-not-found', '-o', 'json', input_text=json.dumps(rendered))
            for obj in kubectl_objects(found_text):
                if obj['metadata'].get('labels', {}).get(OWNER) != self.state['session']:
                    raise NetworkError('unowned_calico_resource', obj['metadata']['name'])
            current = {'phase': 'installing', 'definition': self.definition(), 'images': images,
                       'rendered_sha256': digest(self.output / 'calico-rendered.json')}
            self.state['network'] = current
            self.lab.write_state(self.state)
            try:
                self.prepare_images(images)
                self.lab.kubectl('apply', '--server-side', '--field-manager=agentbox-platform', '-f', '-',
                                 input_text=json.dumps(rendered), timeout=120)
                for kind, name in [('daemonset', 'calico-node'), ('deployment', 'calico-kube-controllers')]:
                    self.lab.kubectl('-n', 'kube-system', 'rollout', 'status', kind + '/' + name, '--timeout=300s', timeout=320)
                # CoreDNS can have exceeded its original rollout deadline while CNI was intentionally absent.
                self.lab.kubectl('-n', 'kube-system', 'wait', '--for=condition=Available', 'deployment/coredns', '--timeout=180s', timeout=200)
                self.lab.kubectl('wait', '--for=condition=Ready', 'nodes', '--all', '--timeout=90s', timeout=110)
                current['components'] = []
                for kind, name in [('daemonset', 'calico-node'), ('deployment', 'calico-kube-controllers')]:
                    obj = json.loads(self.lab.kubectl('-n', 'kube-system', 'get', kind, name, '-o', 'json'))
                    current['components'].append([kind, name, obj['metadata']['uid']])
                current['phase'] = 'ready'
                result = self.verify()
                self.state.update(phase='ready', nodes=self.lab.node_report())
                self.save('network-install.json', result)
                return result
            except BaseException:
                current['phase'] = 'failed'
                raise
            finally:
                self.lab.write_state(self.state)

    def namespace(self, name, role, run_id):
        obj = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': name, 'labels': {
            OWNER: self.state['session'], ROLE: role, 'agentbox.dev/net-run': run_id}}}
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        return {'name': name, 'uid': created['metadata']['uid']}

    def pod(self, name, namespace, role, run_id, image, worker=False, runtime=None):
        obj = json.loads((self.lab.config / 'network-pod.json').read_text()
                         .replace('__NAME__', name).replace('__NAMESPACE__', namespace)
                         .replace('__SESSION__', self.state['session']).replace('__RUN__', run_id)
                         .replace('__ROLE__', role).replace('__IMAGE__', image))
        spec = obj['spec']
        spec['activeDeadlineSeconds'] = 900
        node = 'agentbox-platform-lab-' + ('worker' if worker else 'control-plane')
        spec['nodeSelector'] = {'kubernetes.io/hostname': node}
        if not worker:
            spec['tolerations'] = [{'key': 'node-role.kubernetes.io/control-plane', 'operator': 'Exists', 'effect': 'NoSchedule'}]
        if runtime:
            spec['runtimeClassName'] = runtime
        spec['containers'][0]['command'] = ['/usr/local/bin/python3', '-I', '-B', '-u', '-c',
                                           (self.lab.root / 'scripts/lib/network_server.py').read_text()]
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        return {'name': name, 'namespace': namespace, 'uid': created['metadata']['uid'],
                'role': role, 'run': run_id, 'image': image, 'node': node, 'runtime': runtime}

    def live_pod(self, expected):
        obj = json.loads(self.lab.kubectl('-n', expected['namespace'], 'get', 'pod', expected['name'],
                                          '-o', 'json', announce=False))
        if (obj['metadata']['uid'] != expected['uid'] or
                obj['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
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
        if ipaddress.ip_address(ip) not in ipaddress.ip_network(self.lock['pod_cidr']):
            raise NetworkError('unexpected_pod_network', ip)
        expected['ip'] = ip
        return obj

    def policies(self, namespace, run_id):
        text = (self.lab.config / 'network-policies.json').read_text()
        return json.loads(text.replace('__SANDBOX_NS__', namespace).replace('__RUN__', run_id)
                          .replace('__SESSION__', self.state['session']))['items']

    def apply_policies(self, policies):
        value = {'apiVersion': 'v1', 'kind': 'List', 'items': policies}
        self.lab.kubectl('create', '-f', '-', input_text=json.dumps(value))

    def delete_policies(self, namespace, names, run_id):
        for name in names:
            value = json.loads(self.lab.kubectl('-n', namespace, 'get', 'networkpolicy', name, '-o', 'json'))
            if (value['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
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
        program = (self.lab.root / 'scripts/lib/network_client.py').read_text()
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
            self.save('network-test.json', record)
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
            if (obj['metadata']['uid'] != namespace['uid'] or obj['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                    obj['metadata']['labels'].get('agentbox.dev/net-run') != run_id):
                raise NetworkError('unowned_network_namespace', namespace['name'])
            self.lab.kubectl('delete', 'namespace', namespace['name'], '--wait=true', '--timeout=90s', timeout=110)
        for namespace in namespaces.values():
            if self.lab.kubectl('get', 'namespace', namespace['name'], '--ignore-not-found', '-o', 'json').strip():
                raise NetworkError('network_cleanup_incomplete', namespace['name'])

    def test(self, image):
        with self.lab.operation_lock():
            self.attach(ready=True)
            self.verify()
            runtime = GVisor(self.lab)
            runtime.attach()
            runtime.verify_installation()
            run_id = uuid.uuid4().hex[:10]
            record = {'run': run_id, 'session': self.state['session'], 'phases': {}, 'cleanup_complete': False}
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
                self.save('network-test.json', record)
                for namespace in namespaces.values():
                    self.lab.kubectl('-n', namespace['name'], 'wait', '--for=condition=Ready', 'pods', '--all', '--timeout=240s', timeout=260)
                for pod in pods.values():
                    self.live_pod(pod)
                record['gvisor_runtime'] = runtime.runtime_evidence(self.live_pod(pods['sandbox-gvisor']))
                api = json.loads(self.lab.kubectl('-n', 'default', 'get', 'service', 'kubernetes', '-o', 'json'))['spec']['clusterIP']
                cases = self.matrix(pods, api)
                record['cases'] = cases
                policies = self.policies(namespaces['sandbox']['name'], run_id)
                self.save('network-policies.json', policies)
                print('[platform] network phase: baseline (all listeners reachable)', flush=True)
                self.converge(pods, cases, 'baseline', run_id, record)
                self.apply_policies(policies[:1])
                print('[platform] network phase: default deny', flush=True)
                self.converge(pods, cases, 'deny', run_id, record)
                self.apply_policies(policies[1:])
                print('[platform] network phase: selected control/data/DNS allowed', flush=True)
                self.converge(pods, cases, 'allow', run_id, record)
                self.delete_policies(namespaces['sandbox']['name'], [p['metadata']['name'] for p in policies[1:]], run_id)
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
                self.save('network-test.json', record)


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
