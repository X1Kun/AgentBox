"""Install the upstream controller and validate cold sessions with its Go SDK."""
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import shutil
import tempfile
import time
import uuid

from lib.platform_gvisor import GVisor, digest
from lib.platform_network import Network, kubectl_objects, OWNER

CONTROLLER_NS = 'agent-sandbox-system'
RUN = 'agentbox.dev/session-run'
ROLE = 'agentbox.dev/session-role'
EXPECTED_CHECKS = {
    'client_rbac_scope', 'multi_call_file_persistence', 'cross_session_files_isolated',
    'program_failure_does_not_end_session', 'fresh_python_interpreter_per_command',
    'nonroot_readonly_no_cluster_token', 'file_api_workspace_boundary',
    'peer_and_cluster_api_egress_blocked', 'stable_claim_sandbox_pod_service_identity',
    'explicit_close_and_reject_new_calls', 'controller_claim_expiry_and_gc',
    'deadline_no_replay_and_session_cleanup', 'close_running_parent_and_child',
}
WARM_CHECKS = {
    'client_rbac_scope', 'fixed_two_ready_members_before_claims',
    'concurrent_claims_keep_distinct_precreated_pods', 'parallel_warm_sessions_keep_files_isolated',
    'pool_replenishes_within_total_pod_budget', 'exhaustion_is_bounded_without_creation_replay',
    'used_sessions_deleted_and_closed_handles_rejected', 'capacity_recovers_with_clean_new_environment',
    'fixed_inventory_restored_without_used_members',
}


def warm_fixture(template, namespace, owner, run_id):
    """Fixed behavior fixture, separate from the cold profile and product policy."""
    pool = next(o for o in template['items'] if o['kind'] == 'SandboxWarmPool')
    pool['metadata']['name'] = 'python-warm'
    pool['spec']['replicas'] = 2
    quota = {'apiVersion': 'v1', 'kind': 'ResourceQuota',
             'metadata': {'name': 'session-budget', 'namespace': namespace, 'labels': {OWNER: owner, RUN: run_id}},
             'spec': {'hard': {'pods': '3', 'requests.cpu': '300m', 'requests.memory': '384Mi',
                               'limits.cpu': '1500m', 'limits.memory': '1536Mi'}}}
    return quota


class SessionError(Exception):
    def __init__(self, reason, detail):
        super().__init__(detail)
        self.reason = reason


def checked_source(canonical, supplied):
    value = supplied or canonical
    if (not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}', value) or
            '://' in value or '..' in value or value.split('@')[1] != canonical.split('@')[1]):
        raise SessionError('session_image_digest_mismatch', 'mirror must preserve the locked image digest')
    return value


def render_controller(objects, lock, owner, image):
    if len(objects) != lock['object_count']:
        raise SessionError('unexpected_session_manifest', 'upstream bundle object count changed')
    deployments = []
    for obj in objects:
        obj['metadata'].setdefault('labels', {}).update({OWNER: owner, 'app.kubernetes.io/managed-by': 'agentbox-platform'})
        if obj['kind'] != 'Deployment':
            continue
        deployments.append(obj)
        if obj['metadata']['name'] != 'agent-sandbox-controller' or obj['metadata'].get('namespace') != CONTROLLER_NS:
            raise SessionError('unexpected_session_controller', obj['metadata']['name'])
        spec = obj['spec']['template']['spec']
        if len(spec['containers']) != 1 or spec['containers'][0]['image'] != 'registry.k8s.io/agent-sandbox/agent-sandbox-controller:' + lock['version']:
            raise SessionError('unexpected_session_image', 'controller image does not match release')
        spec['nodeSelector'] = {'kubernetes.io/hostname': 'agentbox-platform-lab-control-plane'}
        spec['tolerations'] = [{'key': 'node-role.kubernetes.io/control-plane', 'operator': 'Exists', 'effect': 'NoSchedule'}]
        container = spec['containers'][0]
        container.update(image=image, imagePullPolicy='Never',
                         resources={'requests': {'cpu': '100m', 'memory': '128Mi'}, 'limits': {'cpu': '1000m', 'memory': '512Mi'}},
                         readinessProbe={'httpGet': {'path': '/readyz', 'port': 8081}, 'periodSeconds': 2, 'failureThreshold': 60})
    if len(deployments) != 1:
        raise SessionError('unexpected_session_manifest', 'expected one combined core/extensions controller')
    return {'apiVersion': 'v1', 'kind': 'List', 'items': objects}


class Sessions:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.lock = json.loads((lab.config / 'session.lock.json').read_text())
        if self.lock.get('schema_version') != 1 or self.lock.get('warm_replicas') != 0:
            raise SessionError('invalid_session_lock', 'this phase supports cold creation only')
        self.bundle_path = lab.state_dir / 'session-bundle.json'
        self.state = None

    def save(self, name, value):
        (self.output / name).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')

    def definition(self):
        names = ('session.lock.json', 'session-template.json', 'session-rbac.json', 'session-client-job.json')
        return {name: digest(self.lab.config / name) for name in names}

    def build_inputs(self):
        files = [self.lab.root / 'go.mod', self.lab.root / 'go.sum',
                 self.lab.config / 'session.lock.json', self.lab.config / 'sandbox-runtime.Dockerfile',
                 self.lab.config / 'session-client.Dockerfile']
        for folder in ('cmd/agentbox-session-smoke', 'internal/sessionprobe'):
            files += [p for p in (self.lab.root / folder).glob('*.go') if not p.name.endswith('_test.go')]
        return {str(p.relative_to(self.lab.root)): digest(p) for p in sorted(files)}

    def docker_image(self, name):
        return json.loads(self.lab.runner.run([self.lab.tools['docker'], 'image', 'inspect', name]))[0]

    def build(self, controller_source=None, python_source=None):
        controller_source = checked_source(self.lock['controller_image'], controller_source)
        python_image = self.lab.lock['smoke']['image']
        python_source = checked_source(python_image, python_source)
        with self.lab.operation_lock():
            docker = self.lab.docker_info()
            version = self.lab.runner.run(['go', 'version'])
            if self.lock['go_toolchain'] not in version:
                raise SessionError('go_toolchain_mismatch', 'use the pinned toolchain selected by go.mod')
            module = json.loads(self.lab.runner.run(['go', 'list', '-m', '-json', self.lock['module']]))
            if module.get('Version') != self.lock['version'] or module.get('Sum') != self.lock['module_sum'] or module.get('Replace'):
                raise SessionError('session_sdk_mismatch', 'Go module version/checksum or replacement differs')
            inputs = self.build_inputs()
            fingerprint = hashlib.sha256(json.dumps(inputs, sort_keys=True).encode()).hexdigest()
            tags = {'controller': 'agentbox/session-controller:' + self.lock['controller_image'].split(':')[-1][:16],
                    'runtime': 'agentbox/session-runtime:' + fingerprint[:16],
                    'client': 'agentbox/session-client:' + fingerprint[:16]}
            for image in (controller_source, python_source):
                self.lab.prepare_image(image)
            original = self.docker_image(controller_source)
            self.lab.runner.run([self.lab.tools['docker'], 'tag', controller_source, tags['controller']])
            if self.docker_image(tags['controller'])['Id'] != original['Id']:
                raise SessionError('session_controller_identity_changed', 'local controller alias differs')
            directory = self.lab.state_dir / ('session-build-' + fingerprint[:16])
            directory.mkdir(parents=True, exist_ok=True)
            binary_hashes = {}
            for name, package, flags in [
                ('sandboxd', self.lock['module'] + '/packages/sandboxd/cmd/sandboxd',
                 '-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=' + self.lock['version']),
                ('agentbox-session-smoke', './cmd/agentbox-session-smoke', '-s -w')]:
                path = directory / name
                self.lab.runner.run(['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH=amd64',
                                     'go', 'build', '-p=2', '-mod=readonly', '-trimpath', '-buildvcs=false',
                                     '-ldflags=' + flags, '-o', str(path), package], timeout=900)
                binary_hashes[name] = digest(path)
            # A minimal build context avoids sending repository credentials/cache to Docker.
            for kind, dockerfile, binary in [('runtime', 'sandbox-runtime.Dockerfile', 'sandboxd'),
                                             ('client', 'session-client.Dockerfile', 'agentbox-session-smoke')]:
                with tempfile.TemporaryDirectory(prefix='session-image-', dir=self.lab.state_dir) as temporary:
                    context = Path(temporary)
                    shutil.copyfile(self.lab.config / dockerfile, context / 'Dockerfile')
                    shutil.copy2(directory / binary, context / binary)
                    self.lab.runner.run([self.lab.tools['docker'], 'build', '--platform=linux/amd64',
                                         '--build-arg', 'PYTHON_IMAGE=' + python_source,
                                         '--label', 'agentbox.dev/build=' + fingerprint,
                                         '-t', tags[kind], str(context)], timeout=300)
            images = {kind: {'name': name, 'id': self.docker_image(name)['Id']} for kind, name in tags.items()}
            result = {'schema_version': 1, 'docker_id': docker['ID'], 'inputs': inputs, 'fingerprint': fingerprint,
                      'images': images, 'binary_sha256': binary_hashes, 'go_version': version.strip(),
                      'sdk_version': module['Version'], 'sdk_sum': module['Sum'],
                      'controller_source': controller_source, 'python_source': python_source}
            self.lab.write_json(self.bundle_path, result)
            self.save('session-build.json', result)
            return result

    def bundle(self):
        value = json.loads(self.bundle_path.read_text())
        if value['inputs'] != self.build_inputs():
            raise SessionError('session_bundle_changed', 'run session-build for the current Go and image inputs')
        if value['docker_id'] != self.lab.docker_info()['ID']:
            raise SessionError('session_bundle_daemon_changed', 'bundle belongs to another Docker daemon')
        for image in value['images'].values():
            if self.docker_image(image['name'])['Id'] != image['id']:
                raise SessionError('session_image_changed', image['name'])
        return value

    def attach(self):
        network = Network(self.lab)
        network.attach(ready=True)
        network.verify()
        self.state = network.state
        self.control_plane_ready()
        runtime = GVisor(self.lab)
        runtime.attach()
        runtime.verify_installation()
        return runtime

    def control_plane_ready(self):
        expected = {'kube-controller-manager', 'kube-scheduler'}
        pods = json.loads(self.lab.kubectl('-n', 'kube-system', 'get', 'pods', '-l',
                         'component in (kube-controller-manager,kube-scheduler)', '--field-selector',
                         'spec.nodeName=' + self.state['cluster'] + '-control-plane', '-o', 'json'))['items']
        components = []
        for pod in pods:
            components.append({'component': pod['metadata'].get('labels', {}).get('component'),
                               'pod': pod['metadata']['name'], 'uid': pod['metadata'].get('uid'),
                               'ready': any(c.get('type') == 'Ready' and c.get('status') == 'True'
                                            for c in pod.get('status', {}).get('conditions', [])),
                               'phase': pod.get('status', {}).get('phase'),
                               'waiting': [c.get('state', {}).get('waiting', {}).get('reason')
                                           for c in pod.get('status', {}).get('containerStatuses', [])]})
        self.save('session-control-plane.json', {'components': components})
        if len(components) != 2 or {c['component'] for c in components} != expected or not all(c['ready'] for c in components):
            raise SessionError('session_control_plane_not_ready', json.dumps(components))

    def image_archive(self, bundle):
        """Separate export from node import; retain a verified archive for retries."""
        archive = self.lab.state_dir / ('session-images-' + bundle['fingerprint'] + '.tar')
        metadata = archive.with_suffix('.json')
        expected = {'docker_id': bundle['docker_id'], 'images': bundle['images']}
        if archive.exists() and metadata.exists():
            cached = json.loads(metadata.read_text())
            if (any(cached.get(k) != v for k, v in expected.items()) or
                    cached.get('sha256') != digest(archive)):
                raise SessionError('session_archive_changed', 'cached export identity or checksum differs')
            self.save('session-image-archive.json', dict(cached, reused=True))
            return archive
        with tempfile.TemporaryDirectory(prefix='session-export-', dir=self.lab.state_dir) as temporary:
            path = Path(temporary) / 'images.tar'
            self.lab.runner.run([self.lab.tools['docker'], 'save', '-o', str(path),
                                 *[i['name'] for i in bundle['images'].values()]], timeout=600)
            checksum = digest(path)
            path.replace(archive)
        value = dict(expected, sha256=checksum)
        self.lab.write_json(metadata, value)
        self.save('session-image-archive.json', dict(value, reused=False))
        return archive

    def node_images_match(self, bundle):
        evidence = {}
        complete = True
        for node in self.state['node_ids']:
            images = json.loads(self.lab.runner.run([self.lab.tools['docker'], 'exec', node,
                                                     'crictl', 'images', '-o', 'json']))['images']
            matched = {}
            for role, expected in bundle['images'].items():
                tag = 'docker.io/' + expected['name']
                found = [v for v in images if tag in v.get('repoTags', []) and v['id'] == expected['id']]
                matched[role] = expected['id'] if len(found) == 1 else None
            evidence[node] = matched
            complete = complete and all(matched.values())
        self.save('session-node-images.json', {'complete': bool(complete), 'nodes': evidence})
        return bool(complete)

    def load_images(self, bundle):
        if self.node_images_match(bundle):
            return
        for node in self.state['node_ids']:
            processes = self.lab.runner.run([self.lab.tools['docker'], 'exec', node, 'ps', '-eo', 'comm='])
            if 'ctr' in processes.split():
                raise SessionError('session_import_in_progress', 'wait for the existing node ctr operation before retrying import')
        archive = self.image_archive(bundle)
        self.lab.runner.run([self.lab.tools['kind'], 'load', 'image-archive', str(archive),
                             '--name', self.state['cluster']], timeout=300)
        # Containerd import completion precedes the CRI image cache update.
        until = time.monotonic() + 60
        while time.monotonic() < until:
            if self.node_images_match(bundle):
                return
            time.sleep(.5)
        raise SessionError('session_image_load_incomplete', 'node image IDs did not converge to the verified bundle within 60s')

    def verify(self, bundle):
        installed = self.state.get('sessions', {})
        if installed.get('phase') != 'ready' or installed.get('definition') != self.definition() or installed.get('bundle_fingerprint') != bundle['fingerprint']:
            raise SessionError('sessions_not_ready', 'install the current upstream session definition first')
        controller = json.loads(self.lab.kubectl('-n', CONTROLLER_NS, 'get', 'deployment', 'agent-sandbox-controller', '-o', 'json'))
        if (controller['metadata']['uid'] != installed['controller_uid'] or
                controller['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                controller['spec']['template']['spec']['containers'][0]['image'] != bundle['images']['controller']['name'] or
                controller.get('status', {}).get('availableReplicas', 0) != 1):
            raise SessionError('session_controller_changed', 'controller ownership, image or readiness changed')
        for name, uid in installed['crds'].items():
            crd = json.loads(self.lab.kubectl('get', 'crd', name, '-o', 'json'))
            if crd['metadata']['uid'] != uid or not any(v['name'] == 'v1beta1' and v.get('served') for v in crd['spec']['versions']):
                raise SessionError('session_crd_changed', name)
        return {'controller_uid': installed['controller_uid'], 'crds': installed['crds'], 'bundle_fingerprint': bundle['fingerprint']}

    def install(self, source):
        data = Path(source).read_bytes()
        if hashlib.sha256(data).hexdigest() != self.lock['manifest_sha256']:
            raise SessionError('session_manifest_checksum_mismatch', 'upstream release YAML differs')
        with self.lab.operation_lock():
            self.attach()
            bundle = self.bundle()
            previous = self.state.get('sessions')
            if previous and (previous['definition'] != self.definition() or previous['bundle_fingerprint'] != bundle['fingerprint']):
                raise SessionError('session_definition_changed', 'recreate instead of replacing a live controller/runtime profile')
            if previous and previous.get('phase') == 'ready':
                return dict(already_installed=True, **self.verify(bundle))
            self.load_images(bundle)
            path = self.output / 'session-upstream.yaml'
            path.write_bytes(data)
            objects = kubectl_objects(self.lab.kubectl('create', '--dry-run=client', '--validate=false', '-f', path, '-o', 'json', timeout=60))
            rendered = render_controller(objects, self.lock, self.state['session'], bundle['images']['controller']['name'])
            self.save('session-controller-rendered.json', rendered)
            found = kubectl_objects(self.lab.kubectl('get', '-f', '-', '--ignore-not-found', '-o', 'json', input_text=json.dumps(rendered)))
            if any(o['metadata'].get('labels', {}).get(OWNER) != self.state['session'] for o in found):
                raise SessionError('unowned_session_resource', 'never adopt foreign upstream controller resources')
            installed = {'phase': 'installing', 'definition': self.definition(), 'bundle_fingerprint': bundle['fingerprint']}
            self.state['sessions'] = installed
            self.lab.write_state(self.state)
            try:
                self.lab.kubectl('apply', '--server-side', '--field-manager=agentbox-platform', '-f', '-', input_text=json.dumps(rendered), timeout=120)
                crds = ['sandboxes.agents.x-k8s.io', 'sandboxclaims.extensions.agents.x-k8s.io',
                        'sandboxtemplates.extensions.agents.x-k8s.io', 'sandboxwarmpools.extensions.agents.x-k8s.io']
                self.lab.kubectl('wait', '--for=condition=Established', *['crd/' + n for n in crds], '--timeout=60s', timeout=80)
                self.lab.kubectl('-n', CONTROLLER_NS, 'rollout', 'status', 'deployment/agent-sandbox-controller', '--timeout=180s', timeout=200)
                controller = json.loads(self.lab.kubectl('-n', CONTROLLER_NS, 'get', 'deployment', 'agent-sandbox-controller', '-o', 'json'))
                installed.update(phase='ready', controller_uid=controller['metadata']['uid'], crds={})
                for name in crds:
                    obj = json.loads(self.lab.kubectl('get', 'crd', name, '-o', 'json'))
                    installed['crds'][name] = obj['metadata']['uid']
                result = self.verify(bundle)
                self.save('session-install.json', result)
                return result
            except BaseException:
                installed['phase'] = 'failed'
                raise
            finally:
                self.lab.write_state(self.state)

    def namespace(self, name, role, run_id):
        obj = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': name, 'labels': {
            OWNER: self.state['session'], RUN: run_id, ROLE: role}}}
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        return {'name': name, 'uid': created['metadata']['uid']}

    def render(self, name, params):
        text = (self.lab.config / name).read_text()
        for key, value in params.items():
            text = text.replace('__' + key + '__', value)
        obj = json.loads(text)
        objects = obj['items'] if obj.get('kind') == 'List' else [obj]
        for item in objects:
            item['metadata'].setdefault('labels', {}).update({OWNER: self.state['session'], RUN: params['RUN']})
        return obj

    def create(self, obj):
        return kubectl_objects(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))

    def client_policy(self, client_ns, work_ns, run_id):
        api = json.loads(self.lab.kubectl('-n', 'default', 'get', 'service', 'kubernetes', '-o', 'json'))
        slices = json.loads(self.lab.kubectl('-n', 'default', 'get', 'endpointslices', '-l',
                                           'kubernetes.io/service-name=kubernetes', '-o', 'json'))['items']
        dns = {'to': [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'kube-system'}},
                       'podSelector': {'matchLabels': {'k8s-app': 'kube-dns'}}}],
               'ports': [{'protocol': 'UDP', 'port': 53}, {'protocol': 'TCP', 'port': 53}]}
        rules = [dns, {'to': [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': work_ns}},
                              'podSelector': {'matchLabels': {ROLE: 'runtime', RUN: run_id}}}],
                       'ports': [{'protocol': 'TCP', 'port': 8080}, {'protocol': 'TCP', 'port': 9090}]}]
        api_ip = api['spec']['clusterIP']
        ipaddress.IPv4Address(api_ip)
        rules.append({'to': [{'ipBlock': {'cidr': api_ip + '/32'}}], 'ports': [{'protocol': 'TCP', 'port': 443}]})
        # Different CNIs may evaluate service traffic before or after DNAT.
        for item in slices:
            for endpoint in item['endpoints']:
                for address in endpoint['addresses']:
                    ipaddress.IPv4Address(address)
                    rules.append({'to': [{'ipBlock': {'cidr': address + '/32'}}],
                                  'ports': [{'protocol': 'TCP', 'port': p['port']} for p in item['ports']]})
        return {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                'metadata': {'name': 'session-client', 'namespace': client_ns, 'labels': {OWNER: self.state['session'], RUN: run_id}},
                'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress'], 'ingress': [], 'egress': rules}}

    def observer(self, namespace, run_id, image):
        params = {'NAME': 'observer', 'NAMESPACE': namespace, 'SESSION': self.state['session'],
                  'RUN': run_id, 'ROLE': 'observer', 'IMAGE': image}
        obj = self.render('network-pod.json', params)
        obj['metadata']['labels'][RUN] = run_id
        spec = obj['spec']
        spec.update(activeDeadlineSeconds=900, nodeSelector={'kubernetes.io/hostname': 'agentbox-platform-lab-control-plane'},
                    tolerations=[{'key': 'node-role.kubernetes.io/control-plane', 'operator': 'Exists', 'effect': 'NoSchedule'}])
        spec['containers'][0].update(imagePullPolicy='Never', command=['/usr/local/bin/python3', '-I', '-B', '-u', '-c',
                                       (self.lab.root / 'scripts/lib/network_server.py').read_text()])
        value = self.create(obj)[0]
        self.lab.kubectl('-n', namespace, 'wait', '--for=condition=Ready', 'pod/observer', '--timeout=120s', timeout=140)
        return {'namespace': namespace, 'name': 'observer', 'uid': value['metadata']['uid']}

    def inspect_identity(self, identity, namespace, run_id, runtime):
        values = {}
        for resource, name, uid in [('sandboxclaim', identity['claim'], identity['claim_uid']),
                                    ('sandbox', identity['sandbox'], identity['sandbox_uid']),
                                    ('pod', identity['pod'], identity['pod_uid']),
                                    ('service', identity['service'], identity['service_uid'])]:
            obj = json.loads(self.lab.kubectl('-n', namespace, 'get', resource, name, '-o', 'json'))
            if obj['metadata']['uid'] != uid:
                raise SessionError('session_identity_changed', name)
            values[resource] = obj
        pod = values['pod']
        if (pod['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                pod['metadata']['labels'].get(RUN) != run_id or pod['spec'].get('runtimeClassName') != 'agentbox-gvisor' or
                pod['spec'].get('automountServiceAccountToken', True) or pod['status'].get('phase') != 'Running'):
            raise SessionError('session_pod_boundary_changed', identity['pod'])
        return runtime.runtime_evidence(pod, memory_bytes=512 * 1024 * 1024)

    def outsider_blocked(self, observer, identities, run_id, record):
        program = (self.lab.root / 'scripts/lib/network_client.py').read_text()
        cases = [{'id': i['pod_uid'] + ':' + str(port), 'protocol': 'tcp', 'ip': i['pod_ip'], 'port': port}
                 for i in identities for port in (8080, 9090)]
        until = time.monotonic() + 45
        consecutive = 0
        samples = []
        while time.monotonic() < until:
            output = json.loads(self.lab.kubectl('-n', observer['namespace'], 'exec', observer['name'], '-c', 'compute',
                                  '--', '/usr/local/bin/python3', '-I', '-B', '-c', program, json.dumps(cases),
                                  timeout=15, announce=False))
            if output.get('pod_uid') != observer['uid'] or output.get('run') != run_id:
                raise SessionError('observer_identity_changed', 'untrusted observer was replaced')
            if len(output['results']) != len(cases) or {v['id'] for v in output['results']} != {c['id'] for c in cases}:
                raise SessionError('invalid_observer_response', 'incomplete session ingress probes')
            samples.append(output['results'])
            consecutive = consecutive + 1 if all(v['kind'] == 'connect_timeout' for v in output['results']) else 0
            if consecutive >= 2:
                record['outsider_ingress'] = samples
                return
            time.sleep(.5)
        record['outsider_ingress'] = samples
        raise SessionError('session_ingress_not_isolated', 'untrusted namespace can reach the session runtime')

    def policies_ready(self, namespace, template_uid):
        until = time.monotonic() + 30
        while time.monotonic() < until:
            policies = json.loads(self.lab.kubectl('-n', namespace, 'get', 'networkpolicies', '-o', 'json', announce=False))['items']
            if len(policies) == 1 and any(r.get('uid') == template_uid for r in policies[0]['metadata'].get('ownerReferences', [])):
                if set(policies[0]['spec'].get('policyTypes', [])) == {'Ingress', 'Egress'}:
                    self.save('controller-managed-policy.json', policies[0])
                    return
            time.sleep(.5)
        raise SessionError('session_policy_not_ready', 'expected one Template-owned managed ingress/egress policy')

    def close_namespaces(self, namespaces, run_id):
        for value in namespaces.values():
            obj = json.loads(self.lab.kubectl('get', 'namespace', value['name'], '-o', 'json'))
            if (obj['metadata']['uid'] != value['uid'] or obj['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                    obj['metadata']['labels'].get(RUN) != run_id):
                raise SessionError('unowned_session_namespace', value['name'])
            self.lab.kubectl('delete', 'namespace', value['name'], '--wait=true', '--timeout=90s', timeout=110)

    def warm_quota_ready(self, namespace):
        until = time.monotonic() + 60
        while time.monotonic() < until:
            quota = json.loads(self.lab.kubectl('-n', namespace, 'get', 'resourcequota', 'session-budget', '-o', 'json', announce=False))
            if quota.get('status', {}).get('hard') == quota['spec']['hard']:
                return quota
            time.sleep(.5)
        raise SessionError('warm_quota_not_ready', 'ResourceQuota has not observed the fixed budget')

    def inspect_supply(self, supply, pool_uid, namespace, run_id, runtime, record):
        for identity in supply:
            sandbox = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandbox', identity['sandbox'], '-o', 'json'))
            pod = json.loads(self.lab.kubectl('-n', namespace, 'get', 'pod', identity['pod'], '-o', 'json'))
            if (sandbox['metadata']['uid'] != identity['sandbox_uid'] or pod['metadata']['uid'] != identity['pod_uid'] or
                    not any(r.get('uid') == pool_uid and r.get('controller') for r in sandbox['metadata'].get('ownerReferences', [])) or
                    not any(r.get('uid') == identity['sandbox_uid'] and r.get('controller') for r in pod['metadata'].get('ownerReferences', [])) or
                    pod['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                    pod['metadata']['labels'].get(RUN) != run_id or pod['spec'].get('automountServiceAccountToken', True)):
                raise SessionError('warm_supply_identity_changed', identity['pod'])
            record['runtime_evidence'][identity['pod_uid']] = runtime.runtime_evidence(pod, memory_bytes=512 * 1024 * 1024)

    def observe_exhaustion(self, namespace, run_id, used, record):
        claims = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandboxclaims', '-o', 'json', announce=False))['items']
        remaining = [c for c in claims if c['metadata']['uid'] not in {v['claim_uid'] for v in used}]
        if len(remaining) > 1:
            raise SessionError('warm_attempt_replayed', 'more than one extra Claim exists')
        if not remaining:
            return
        claim = remaining[0]
        if claim['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or claim['metadata']['labels'].get(RUN) != run_id:
            raise SessionError('unowned_warm_attempt', claim['metadata']['name'])
        previous = record.get('exhausted_claim')
        identity = {'name': claim['metadata']['name'], 'uid': claim['metadata']['uid']}
        if previous and previous != identity:
            raise SessionError('warm_attempt_replayed', 'extra Claim identity changed')
        record['exhausted_claim'] = identity
        objects = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandboxes', '-o', 'json', announce=False))['items']
        for sandbox in objects:
            if not any(r.get('uid') == identity['uid'] and r.get('controller') for r in sandbox['metadata'].get('ownerReferences', [])):
                continue
            for condition in sandbox.get('status', {}).get('conditions', []):
                if (condition.get('type') == 'Ready' and condition.get('status') == 'False' and
                        condition.get('observedGeneration') == sandbox['metadata']['generation'] and
                        'exceeded quota: session-budget' in condition.get('message', '') and 'pods' in condition['message']):
                    record['quota_rejection'] = {'claim': identity, 'sandbox': {'name': sandbox['metadata']['name'], 'uid': sandbox['metadata']['uid']},
                                                 'condition': condition}

    def verify_exhaustion_rollback(self, namespace, event, record):
        proof = record.get('quota_rejection')
        if not proof:
            raise SessionError('warm_quota_rejection_missing', 'a timeout alone does not prove quota exhaustion')
        quota = json.loads(self.lab.kubectl('-n', namespace, 'get', 'resourcequota', 'session-budget', '-o', 'json'))
        if (quota['metadata']['uid'] != event['warm']['quota_uid'] or quota['spec']['hard']['pods'] != '3' or
                quota.get('status', {}).get('used', {}).get('pods') != '3'):
            raise SessionError('warm_budget_changed', 'expected three active Pods at exhaustion')
        until = time.monotonic() + 90
        while time.monotonic() < until:
            remaining = []
            for kind in ('sandboxclaim', 'sandbox'):
                identity = proof['claim' if kind == 'sandboxclaim' else 'sandbox']
                output = self.lab.kubectl('-n', namespace, 'get', kind, identity['name'], '--ignore-not-found', '-o', 'json', announce=False)
                if output.strip():
                    obj = json.loads(output)
                    if obj['metadata']['uid'] != identity['uid']:
                        raise SessionError('warm_attempt_identity_changed', identity['name'])
                    remaining.append(kind)
            for kind in ('pods', 'services'):
                objects = json.loads(self.lab.kubectl('-n', namespace, 'get', kind, '-o', 'json', announce=False))['items']
                remaining += [kind for obj in objects if any(r.get('uid') == proof['sandbox']['uid'] for r in obj['metadata'].get('ownerReferences', []))]
            if not remaining:
                record['failed_open_cleanup_confirmed'] = True
                record['quota_at_exhaustion'] = quota
                return
            time.sleep(.5)
        raise SessionError('warm_attempt_cleanup_incomplete', 'failed Open left Claim-owned resources')

    def stop_warm_pool(self, namespace, event):
        pool = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandboxwarmpool', 'python-warm', '-o', 'json'))
        if pool['metadata']['uid'] != event['warm']['pool_uid'] or pool['metadata'].get('labels', {}).get(OWNER) != self.state['session']:
            raise SessionError('unowned_warm_pool', 'pool identity changed before cleanup')
        self.lab.kubectl('-n', namespace, 'delete', 'sandboxwarmpool', 'python-warm', '--cascade=foreground', '--wait=true', '--timeout=90s', timeout=110)
        for kind in ('sandboxclaims', 'sandboxes', 'pods', 'services'):
            values = json.loads(self.lab.kubectl('-n', namespace, 'get', kind, '-o', 'json'))['items']
            if values:
                raise SessionError('warm_pool_cleanup_incomplete', kind + ' remain after pool shutdown')

    def send_marker(self, namespace, pod_name, marker, record):
        try:
            self.lab.kubectl('-n', namespace, 'exec', pod_name, '-c', 'client', '--',
                             '/usr/local/bin/python3', '-I', '-B', '-c',
                             "from pathlib import Path; Path('/work/" + marker + "').touch()")
        except Exception as error:
            # The final marker lets PID 1 exit, which can kill this exec before
            # its status is delivered. Do not replay it. The caller still must
            # observe the matching complete event and successful Job, then
            # independently verify all runtime and namespace cleanup.
            commands = self.lab.runner.commands
            code = commands[-1].get('exit_code') if commands else None
            if marker != 'warm-finish' or getattr(error, 'reason', None) != 'command_failed' or code != 137:
                raise
            record['final_marker_transport_error'] = {'exit_code': code, 'detail': str(error)}

    def test(self, profile="cold"):
        if profile not in ('cold', 'warm'):
            raise SessionError('invalid_session_profile', profile)
        with self.lab.operation_lock():
            runtime = self.attach()
            bundle = self.bundle()
            self.verify(bundle)
            run_id = uuid.uuid4().hex[:10]
            namespaces = {}
            record = {'profile': profile, 'run': run_id, 'owner': self.state['session'], 'namespaces': namespaces,
                      'runtime_evidence': {}, 'go_events': [], 'cleanup_complete': False}
            try:
                roles = [('work', 'agentbox-session-' + run_id), ('client', 'agentbox-session-client-' + run_id)]
                if profile == 'cold':
                    roles.append(('observer', 'agentbox-session-observer-' + run_id))
                for role, name in roles:
                    namespaces[role] = self.namespace(name, role, run_id)
                params = {'OWNER': self.state['session'], 'RUN': run_id, 'WORK_NS': namespaces['work']['name'],
                          'CLIENT_NS': namespaces['client']['name'], 'RUNTIME_IMAGE': bundle['images']['runtime']['name'],
                          'CLIENT_IMAGE': bundle['images']['client']['name']}
                template = self.render('session-template.json', params)
                if profile == 'warm':
                    quota = warm_fixture(template, params['WORK_NS'], self.state['session'], run_id)
                    self.create(quota)
                    record['quota_initial'] = self.warm_quota_ready(params['WORK_NS'])
                self.save('session-template.json', template)
                created = self.create(template)
                template_uid = next(o['metadata']['uid'] for o in created if o['kind'] == 'SandboxTemplate')
                self.create(self.render('session-rbac.json', params))
                self.create(self.client_policy(params['CLIENT_NS'], params['WORK_NS'], run_id))
                self.policies_ready(params['WORK_NS'], template_uid)
                observer = self.observer(namespaces['observer']['name'], run_id, bundle['images']['client']['name']) if profile == 'cold' else None
                job_spec = self.render('session-client-job.json', params)
                job_spec['spec']['template']['spec']['containers'][0]['env'].append({'name': 'SESSION_PROFILE', 'value': profile})
                client_job = self.create(job_spec)[0]
                job_uid = client_job['metadata']['uid']
                record.update(client_job_uid=job_uid, template_uid=template_uid, observer=observer)
                until = time.monotonic() + 800
                handled = set()
                client_pod = None
                complete = None
                while time.monotonic() < until:
                    if profile == 'warm' and 'warm-claimed' in handled and 'warm-exhausted' not in handled:
                        self.observe_exhaustion(params['WORK_NS'], run_id, record['warm_claimed'], record)
                    job = json.loads(self.lab.kubectl('-n', params['CLIENT_NS'], 'get', 'job', 'session-client', '-o', 'json', announce=False))
                    if job['metadata']['uid'] != job_uid:
                        raise SessionError('session_client_identity_changed', 'client Job was replaced')
                    pods = json.loads(self.lab.kubectl('-n', params['CLIENT_NS'], 'get', 'pods', '-l', 'job-name=session-client', '-o', 'json', announce=False))['items']
                    if len(pods) > 1:
                        raise SessionError('unexpected_client_attempts', 'trusted SDK client must not be replayed')
                    if pods:
                        pod = pods[0]
                        if not any(r.get('uid') == job_uid and r.get('controller') for r in pod['metadata'].get('ownerReferences', [])):
                            raise SessionError('session_client_identity_changed', 'Pod is not owned by this Job')
                        if client_pod and client_pod['metadata']['uid'] != pod['metadata']['uid']:
                            raise SessionError('session_client_identity_changed', 'client Pod was replaced')
                        client_pod = pod
                        states = pod.get('status', {}).get('containerStatuses', [])
                        if states and ('running' in states[0].get('state', {}) or 'terminated' in states[0].get('state', {})):
                            logs = self.lab.kubectl('-n', params['CLIENT_NS'], 'logs', pod['metadata']['name'], '-c', 'client',
                                                    '--limit-bytes=65536', announce=False)
                            events = [json.loads(line) for line in logs.splitlines() if line.strip()]
                            record['go_events'] = events
                            (self.output / 'go-client.jsonl').write_text(logs)
                            for event in events:
                                if (event.get('run') != run_id or event.get('owner') != self.state['session'] or
                                        event.get('namespace') != params['WORK_NS'] or event.get('sdk_version') != self.lock['version'] or
                                        event.get('profile') != profile):
                                    raise SessionError('invalid_session_event', 'Go client identity differs')
                                stage = event['stage']
                                if stage in handled:
                                    continue
                                if stage == 'failed':
                                    raise SessionError('go_session_acceptance_failed', event.get('error', 'unknown error'))
                                marker = None
                                if profile == 'warm' and stage == 'warm-prepared':
                                    self.inspect_supply(event['warm']['initial'], event['warm']['pool_uid'], params['WORK_NS'], run_id, runtime, record)
                                    marker = 'warm-start'
                                elif profile == 'warm' and stage == 'warm-claimed':
                                    record['warm_claimed'] = event['sessions']
                                    identity = event['sessions'][2]
                                    record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    marker = 'warm-exhaust'
                                elif profile == 'warm' and stage == 'warm-exhausted':
                                    self.verify_exhaustion_rollback(params['WORK_NS'], event, record)
                                    marker = 'warm-release'
                                elif profile == 'warm' and stage == 'warm-reused':
                                    identity = event['sessions'][3]
                                    record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    marker = 'warm-return'
                                elif profile == 'warm' and stage == 'warm-recovered':
                                    self.inspect_supply(event['warm']['recovered'], event['warm']['pool_uid'], params['WORK_NS'], run_id, runtime, record)
                                    marker = 'warm-finish'
                                elif stage == 'ready':
                                    for identity in event['sessions']:
                                        record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    self.outsider_blocked(observer, event['sessions'], run_id, record)
                                    marker = 'proceed'
                                elif stage == 'timeout-ready':
                                    identity = event['timed_out']
                                    record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    marker = 'timeout'
                                elif stage == 'closing-active':
                                    self.inspect_identity(event['sessions'][0], params['WORK_NS'], run_id, runtime)
                                    marker = 'close'
                                elif stage == 'complete':
                                    complete = event
                                if marker:
                                    print('[platform] ' + profile + ' session checkpoint: ' + stage, flush=True)
                                    self.send_marker(params['CLIENT_NS'], pod['metadata']['name'], marker, record)
                                handled.add(stage)
                    self.save('session-test.json', record)
                    conditions = job.get('status', {}).get('conditions', [])
                    if any(c['type'] == 'Failed' and c['status'] == 'True' for c in conditions):
                        raise SessionError('session_client_job_failed', json.dumps(job['status']))
                    if any(c['type'] == 'Complete' and c['status'] == 'True' for c in conditions):
                        expected = WARM_CHECKS if profile == 'warm' else EXPECTED_CHECKS
                        required = {'warm-prepared', 'warm-claimed', 'warm-exhausted', 'warm-reused', 'warm-recovered'} if profile == 'warm' else {'ready', 'timeout-ready', 'closing-active'}
                        if (complete is None or not complete.get('cleanup_complete') or
                                {c['name'] for c in complete.get('checks', [])} != expected or
                                not all(c['passed'] for c in complete['checks']) or
                                not required.issubset(handled)):
                            raise SessionError('incomplete_session_acceptance', 'client completed without full evidence')
                        break
                    time.sleep(1)
                else:
                    raise SessionError('session_acceptance_timeout', 'trusted SDK client did not complete')
                identities = list(complete['sessions'])
                if profile == 'warm':
                    self.stop_warm_pool(params['WORK_NS'], complete)
                    record['pool_cleanup_complete'] = True
                    identities += complete['warm']['initial'] + complete['warm']['recovered']
                else:
                    identities += [complete['expiring'], complete['timed_out']]
                deadline = time.monotonic() + 60
                while time.monotonic() < deadline:
                    processes = [p for evidence in record['runtime_evidence'].values()
                                 for p in runtime.processes(evidence['sandbox_id'])]
                    cri = json.loads(runtime.node('crictl', 'pods', '-o', 'json'))['items']
                    cri = [s for s in cri if s.get('labels', {}).get('io.kubernetes.pod.uid') in {i['pod_uid'] for i in identities}]
                    if not processes and not cri:
                        record['runtime_cleanup'] = {'remaining_processes': [], 'remaining_cri_sandboxes': []}
                        break
                    time.sleep(1)
                else:
                    raise SessionError('session_runtime_cleanup_incomplete', 'gVisor process or CRI sandbox remains')
                self.close_namespaces(namespaces, run_id)
                record.update(cleanup_complete=True, all_passed=True, checks=complete['checks'])
                return record
            finally:
                self.save('session-test.json', record)
