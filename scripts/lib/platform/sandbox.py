"""Install and verify the frozen upstream Sandbox runtime."""

from lib.core.paths import ROOT as PROJECT_ROOT
from lib.core.errors import SessionError
from lib.manifests import load_lock, render as render_manifest, definition as manifest_definition, lock_identity, render_upstream, python_image_source, build_input_identity, session_definition_matches
from lib.manifests.network import add_api_egress, discover_api_endpoints
import hashlib
import json
from pathlib import Path
import re
import shutil
import tempfile
import time

from lib.platform.gvisor import GVisor, digest
from lib.platform.calico import Network, kubectl_objects, OWNER

CONTROLLER_NS = 'agent-sandbox-system'
RUN = 'agentbox.dev/session-run'
ROLE = 'agentbox.dev/session-role'

# Acceptance source changes do not change the installed upstream runtime.
RUNTIME_BUILD_INPUTS = ('go.mod', 'go.sum', 'deploy/platform/session.lock.json',
                        'deploy/platform/sandbox-runtime.Dockerfile')


def checked_source(canonical, supplied):
    value = supplied or canonical
    if (not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}', value) or
            '://' in value or '..' in value or value.split('@')[1] != canonical.split('@')[1]):
        raise SessionError('session_image_digest_mismatch', 'mirror must preserve the locked image digest')
    return value


def render_controller(objects, lock, owner, image, config=None):
    if len(objects) != lock['object_count']:
        raise SessionError('unexpected_session_manifest', 'upstream bundle object count changed')
    deployments = [o for o in objects if o['kind'] == 'Deployment']
    if len(deployments) != 1:
        raise SessionError('unexpected_session_manifest', 'expected one combined core/extensions controller')
    obj = deployments[0]
    if obj['metadata']['name'] != 'agent-sandbox-controller' or obj['metadata'].get('namespace') != CONTROLLER_NS:
        raise SessionError('unexpected_session_controller', obj['metadata']['name'])
    containers = obj['spec']['template']['spec']['containers']
    if len(containers) != 1 or containers[0]['image'] != 'registry.k8s.io/agent-sandbox/agent-sandbox-controller:' + lock['version']:
        raise SessionError('unexpected_session_image', 'controller image does not match release')
    from lib.manifests import objects as unpack
    rendered = render_upstream(config or PROJECT_ROOT/'deploy',
        'controller-lab', objects, {'OWNER': owner, 'CONTROLLER_IMAGE': image})
    return {'apiVersion': 'v1', 'kind': 'List', 'items': unpack(rendered)}


class Sessions:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.lock = load_lock(lab.config, 'session')
        if self.lock.get('schema_version') != 1 or self.lock.get('warm_replicas') != 0:
            raise SessionError('invalid_session_lock', 'this phase supports cold creation only')
        self.bundle_path = lab.state_dir / 'session-bundle.json'
        self.state = None

    def save(self, name, value):
        (self.output / name).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')

    def definition(self):
        return manifest_definition(self.lab.config, 'session')

    def definition_matches(self, recorded):
        return recorded == self.definition() or session_definition_matches(self.lab.config, recorded)

    def build_inputs(self, runtime_only=True):
        files = [self.lab.root/'go.mod',self.lab.root/'go.sum']
        result = {str(p.relative_to(self.lab.root)):digest(p) for p in sorted(files)}
        result['go.mod'] = build_input_identity(self.lab.config,'go.mod',result['go.mod'])
        # Immutable input IDs of the v1 bundle protocol, not physical filenames.
        # New component paths preserve cache identity only with equal lock/payload values.
        result['deploy/platform/session.lock.json'] = lock_identity(self.lab.config,'session')
        result['deploy/platform/sandbox-runtime.Dockerfile'] = digest(self.lab.config/'components/agent-sandbox/runtime.Dockerfile')
        return result

    def docker_image(self, name):
        return json.loads(self.lab.runner.run([self.lab.tools['docker'], 'image', 'inspect', name]))[0]

    def cached_runtime_binary(self, inputs, docker_id, go_version):
        if not self.bundle_path.is_file():
            return None
        previous = json.loads(self.bundle_path.read_text())
        runtime_inputs = RUNTIME_BUILD_INPUTS
        if (previous.get('docker_id') != docker_id or previous.get('go_version') != go_version.strip() or
                previous.get('sdk_version') != self.lock['version'] or previous.get('sdk_sum') != self.lock['module_sum'] or
                any(previous.get('inputs', {}).get(k) != inputs.get(k) for k in runtime_inputs)):
            return None
        fingerprint = previous.get('fingerprint', '')
        if not re.fullmatch(r'[a-f0-9]{64}', fingerprint):
            raise SessionError('invalid_session_bundle', 'invalid previous build fingerprint')
        binary = self.lab.state_dir / ('session-build-' + fingerprint[:16]) / 'sandboxd'
        if not binary.is_file():
            return None
        if digest(binary) != previous.get('binary_sha256', {}).get('sandboxd'):
            raise SessionError('session_runtime_binary_changed', 'cached upstream runtime binary checksum differs')
        return binary

    def build(self, controller_source=None, python_source=None):
        controller_source = checked_source(self.lock['controller_image'], controller_source)
        python_image = self.lab.lock['smoke']['image']
        python_source = checked_source(python_image, python_source or python_image_source(self.lab.config))
        with self.lab.operation_lock():
            if self.bundle_path.exists():
                try:
                    previous = self.bundle(require_client=False)
                except SessionError as error:
                    if error.reason != 'session_bundle_changed':
                        raise
                else:
                    self.save('session-build.json', dict(previous, runtime_cache_reused=True))
                    return previous
            docker = self.lab.docker_info()
            version = self.lab.runner.run(['go', 'version'])
            if self.lock['go_toolchain'] not in version:
                raise SessionError('go_toolchain_mismatch', 'use the pinned toolchain selected by go.mod')
            module = json.loads(self.lab.runner.run(['go', 'list', '-m', '-json', self.lock['module']]))
            if module.get('Version') != self.lock['version'] or module.get('Sum') != self.lock['module_sum'] or module.get('Replace'):
                raise SessionError('session_sdk_mismatch', 'Go module version/checksum or replacement differs')
            inputs = self.build_inputs(runtime_only=True)
            fingerprint = hashlib.sha256(json.dumps(inputs, sort_keys=True).encode()).hexdigest()
            tags = {'controller': 'agentbox/session-controller:' + self.lock['controller_image'].split(':')[-1][:16],
                    'runtime': 'agentbox/session-runtime:' + fingerprint[:16]}
            for image in (controller_source, python_source):
                self.lab.prepare_image(image)
            original = self.docker_image(controller_source)
            self.lab.runner.run([self.lab.tools['docker'], 'tag', controller_source, tags['controller']])
            if self.docker_image(tags['controller'])['Id'] != original['Id']:
                raise SessionError('session_controller_identity_changed', 'local controller alias differs')
            directory = self.lab.state_dir / ('session-build-' + fingerprint[:16])
            directory.mkdir(parents=True, exist_ok=True)
            binary_hashes = {}
            cached_runtime = self.cached_runtime_binary(inputs, docker['ID'], version)
            reused_binaries = []
            for name, package, flags in [
                ('sandboxd', self.lock['module'] + '/packages/sandboxd/cmd/sandboxd',
                 '-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=' + self.lock['version'])]:
                path = directory / name
                if name == 'sandboxd' and cached_runtime is not None:
                    if cached_runtime != path:
                        shutil.copy2(cached_runtime, path)
                    reused_binaries.append(name)
                else:
                    self.lab.runner.run(['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH=amd64',
                                     'go', 'build', '-p=2', '-mod=readonly', '-trimpath', '-buildvcs=false',
                                     '-ldflags=' + flags, '-o', str(path), package], timeout=900)
                binary_hashes[name] = digest(path)
            # A minimal build context avoids sending repository credentials/cache to Docker.
            for kind, dockerfile, binary in [('runtime', 'components/agent-sandbox/runtime.Dockerfile', 'sandboxd')]:
                with tempfile.TemporaryDirectory(prefix='session-image-', dir=self.lab.state_dir) as temporary:
                    context = Path(temporary)
                    shutil.copyfile(self.lab.config / dockerfile, context / 'Dockerfile')
                    shutil.copy2(directory / binary, context / binary)
                    self.lab.runner.run([self.lab.tools['docker'], 'build', '--platform=linux/amd64',
                                         '--build-arg', 'PYTHON_IMAGE=' + python_source,
                                         '--label', 'agentbox.dev/build=' + fingerprint,
                                         '-t', tags[kind], str(context)], timeout=300)
            images = {kind: {'name': name, 'id': self.docker_image(name)['Id']} for kind, name in tags.items()}
            result = {'schema_version': 2, 'docker_id': docker['ID'], 'inputs': inputs, 'fingerprint': fingerprint,
                      'images': images, 'binary_sha256': binary_hashes, 'go_version': version.strip(),
                      'reused_binaries': reused_binaries,
                      'sdk_version': module['Version'], 'sdk_sum': module['Sum'],
                      'controller_source': controller_source, 'python_source': python_source}
            self.lab.write_json(self.bundle_path, result)
            self.save('session-build.json', result)
            return result

    def bundle(self, require_client=False):
        if require_client:
            raise SessionError("session_client_verification_separate", "use the SDK acceptance client builder")
        value = json.loads(self.bundle_path.read_text())
        current = self.build_inputs(runtime_only=not require_client)
        if not set(RUNTIME_BUILD_INPUTS).issubset(value['inputs']) or not set(RUNTIME_BUILD_INPUTS).issubset(current):
            raise SessionError('session_bundle_changed', 'runtime bundle inputs are incomplete')
        keys = set(current) | set(value['inputs']) if require_client else RUNTIME_BUILD_INPUTS
        if any(value['inputs'].get(key) != current.get(key) for key in keys):
            raise SessionError('session_bundle_changed', 'run session-build for the current Go and image inputs')
        if value['docker_id'] != self.lab.docker_info()['ID']:
            raise SessionError('session_bundle_daemon_changed', 'bundle belongs to another Docker daemon')
        for role, image in value['images'].items():
            if role == 'client' and not require_client:
                continue
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
        if installed.get('phase') != 'ready' or not self.definition_matches(installed.get('definition')) or installed.get('bundle_fingerprint') != bundle['fingerprint']:
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
            bundle = self.bundle(require_client=False)
            previous = self.state.get('sessions')
            if previous and (not self.definition_matches(previous['definition']) or previous['bundle_fingerprint'] != bundle['fingerprint']):
                raise SessionError('session_definition_changed', 'recreate instead of replacing a live controller/runtime profile')
            if previous and previous.get('phase') == 'ready':
                return dict(already_installed=True, **self.verify(bundle))
            runtime_images = {role:image for role,image in bundle['images'].items() if role != 'client'}
            archive_identity = hashlib.sha256(json.dumps(runtime_images,sort_keys=True).encode()).hexdigest()
            self.load_images(dict(bundle, images=runtime_images, fingerprint=bundle['fingerprint']+'-'+archive_identity[:12]))
            # Image unpacking shares the host disk with etcd. Observe recovery
            # before submitting controller resources, never replay that apply.
            self.lab.wait_ready()
            path = self.output / 'session-upstream.yaml'
            path.write_bytes(data)
            objects = kubectl_objects(self.lab.kubectl('create', '--dry-run=client', '--validate=false', '-f', path, '-o', 'json', timeout=60))
            rendered = render_controller(objects, self.lock, self.state['session'], bundle['images']['controller']['name'], self.lab.config)
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
        obj = render_manifest(self.lab.config, 'session-namespace', {'NAME': name, 'ROLE': role, 'RUN': run_id, 'OWNER': self.state['session']})
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        return {'name': name, 'uid': created['metadata']['uid']}

    def render(self, profile, params):
        values = dict(params)
        values.pop('SESSION', None)
        values['OWNER'] = self.state['session']
        return render_manifest(self.lab.config, profile, values)

    def create(self, obj):
        return kubectl_objects(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))

    def client_policy(self, client_ns, work_ns, run_id):
        policy = self.render('client-policy', {'CLIENT_NS': client_ns, 'WORK_NS': work_ns, 'RUN': run_id})
        return add_api_egress(policy, discover_api_endpoints(self.lab.kubectl))


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
            text = self.lab.kubectl('get', 'namespace', value['name'], '--ignore-not-found', '-o', 'json')
            if not text.strip():
                continue
            obj = json.loads(text)
            if (obj['metadata']['uid'] != value['uid'] or obj['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                    obj['metadata']['labels'].get(RUN) != run_id):
                raise SessionError('unowned_session_namespace', value['name'])
            self.lab.kubectl('delete', 'namespace', value['name'], '--wait=true', '--timeout=90s', timeout=110)


