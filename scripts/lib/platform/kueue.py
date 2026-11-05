"""Install and verify the pinned Kueue component."""

from lib.core.validation import require
from lib.core.kubernetes import condition, quantity
import hashlib
import json
from pathlib import Path
import re

import yaml

from lib.platform.gvisor import GVisor, digest
from lib.manifests import UniqueLoader, load_lock, objects, render_upstream, repository_image_source
from lib.platform.calico import Network
from lib.platform.sandbox import Sessions, checked_source

OWNER = 'agentbox.dev/session'
RUN = 'agentbox.dev/batch-run'
MANAGED = 'agentbox.dev/batch-managed'
SYSTEM_NS = 'kueue-system'
CONTROLLER = 'kueue-controller-manager'
API = 'kueue.x-k8s.io/v1beta2'


def resource_budget(value):
    return {part: {key: quantity(amount) for key, amount in resources.items()}
            for part, resources in value.items()}


def webhook_contract(hook):
    """Compare API defaults explicitly, retaining every admission boundary."""
    client = hook['clientConfig']
    service = dict(client.get('service', {}))
    if service:
        service.setdefault('port', 443)
    return {'name': hook['name'], 'rules': [dict(r, scope=r.get('scope', '*')) for r in hook['rules']],
            'namespaceSelector': hook.get('namespaceSelector') or {}, 'objectSelector': hook.get('objectSelector') or {},
            'matchConditions': hook.get('matchConditions', []), 'matchPolicy': hook.get('matchPolicy', 'Equivalent'),
            'failurePolicy': hook.get('failurePolicy', 'Fail'), 'service': service, 'url': client.get('url'),
            'timeoutSeconds': hook.get('timeoutSeconds', 10), 'sideEffects': hook['sideEffects'],
            'admissionReviewVersions': hook['admissionReviewVersions'],
            'reinvocationPolicy': hook.get('reinvocationPolicy', 'Never')}


def checked_manifest(path, lock):
    data = Path(path).read_bytes()
    require(len(data) <= 4 << 20 and hashlib.sha256(data).hexdigest() == lock['manifest_sha256'],
            'kueue_checksum_mismatch', 'manifest differs from the fixed official release')
    values = [v for v in yaml.load_all(data, Loader=UniqueLoader) if v is not None]
    require(len(values) == lock['object_count'], 'unexpected_kueue_manifest', 'upstream object count changed')
    deployments = [v for v in values if v['kind'] == 'Deployment']
    require(len(deployments) == 1 and deployments[0]['metadata']['name'] == CONTROLLER and
            deployments[0]['metadata'].get('namespace') == SYSTEM_NS,
            'unexpected_kueue_manifest', 'expected the official controller Deployment')
    containers = deployments[0]['spec']['template']['spec']['containers']
    require(len(containers) == 1 and containers[0]['image'] == 'registry.k8s.io/kueue/kueue:' + lock['version'],
            'unexpected_kueue_image', 'controller image does not match the pinned release')
    return values


def render_controller(config, upstream, owner, image):
    lock = load_lock(config, 'kueue')
    checked_source(lock['controller_image'], image)
    repository, pinned = image.split('@')
    return render_upstream(config, 'kueue-lab', upstream, {'OWNER': owner},
        [{'name': 'registry.k8s.io/kueue/kueue', 'newName': repository, 'digest': pinned}])


def controller_source(config, supplied=None):
    lock = load_lock(config, 'kueue')
    return checked_source(lock['controller_image'], supplied or
                          repository_image_source(config, 'kueue', lock['controller_image']))


class Kueue:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.lock = load_lock(lab.config, 'kueue')
        require(self.lock.get('schema_version') == 1 and self.lock.get('api_version') == API and
                re.fullmatch(r'v0\.20\.\d+', self.lock.get('version', '')),
                'invalid_kueue_lock', 'unsupported Kueue schema/API')
        checked_source(self.lock['controller_image'], None)
        self.state = None

    def save(self, name, value):
        self.lab.write_json(self.output / name, value)

    def definition(self):
        files = self.lab.config / 'components/kueue'
        return {str(p.relative_to(self.lab.config)): digest(p) for p in sorted(files.glob('*.yaml'))}

    def image_source(self, supplied=None):
        return controller_source(self.lab.config, supplied)

    def attach(self):
        network = Network(self.lab)
        network.attach(ready=True)
        network.verify()
        self.state = network.state
        sessions = Sessions(self.lab)
        sessions.state = self.state
        sessions.control_plane_ready()
        runtime = GVisor(self.lab)
        runtime.attach()
        runtime.verify_installation()
        return runtime

    def inventory(self, resources):
        # Metadata only: the release includes a controller-managed TLS Secret.
        # Never read its data into Runner stdout or the acceptance report.
        fields = '{range .items[*]}{.apiVersion}{"\\t"}{.kind}{"\\t"}{.metadata.namespace}{"\\t"}{.metadata.name}{"\\t"}{.metadata.uid}{"\\t"}{.metadata.labels}{"\\n"}{end}'
        text = self.lab.kubectl('get', '-f', '-', '--ignore-not-found', '-o', 'jsonpath=' + fields,
            input_text=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': resources}), timeout=90, announce=False)
        result = {}
        for line in text.splitlines():
            api, kind, namespace, name, uid, labels = line.split('\t')
            require(json.loads(labels).get(OWNER) == self.state['session'], 'unowned_kueue_resource', kind + '/' + name)
            result['|'.join((api, kind, namespace, name))] = uid
        return result

    def prepare_image(self, image):
        node = self.state['cluster'] + '-control-plane'
        def inventory():
            return json.loads(self.lab.runner.run([self.lab.tools['docker'], 'exec', node,
                'crictl', 'images', '-o', 'json'], announce=False))['images']
        found = [v for v in inventory() if image in v.get('repoDigests', [])]
        cached = bool(found)
        if not found:
            self.lab.runner.run([self.lab.tools['docker'], 'exec', node, 'crictl', 'pull', image], timeout=180)
            found = [v for v in inventory() if image in v.get('repoDigests', [])]
        require(len(found) == 1, 'kueue_image_not_confirmed', 'CRI did not confirm the locked image digest')
        self.save('kueue-image-preparation.json', {'node': node, 'image': image, 'image_id': found[0]['id'], 'cached': cached})

    def get(self, kind, name, namespace=None):
        args = ['-n', namespace] if namespace else []
        text = self.lab.kubectl(*args, 'get', kind, name, '--ignore-not-found', '-o', 'json', announce=False)
        return json.loads(text) if text.strip() else None

    def verify(self):
        current = self.state.get('kueue', {})
        require(current.get('phase') == 'ready' and current.get('definition') == self.definition(),
                'kueue_not_ready', 'install the pinned Kueue component first')
        resources = current['resources']
        require(self.inventory(resources) == current['uids'], 'kueue_identity_changed', 'installed resource UIDs changed')
        configuration = current['configuration']
        deployment = self.get('deployment', CONTROLLER, SYSTEM_NS)
        require(deployment is not None and condition(deployment, 'Available') and
                deployment.get('status', {}).get('observedGeneration', 0) >= deployment['metadata']['generation'],
                'kueue_not_ready', 'controller Deployment is not available')
        pod = deployment['spec']['template']['spec']
        expected = next(v for v in configuration if v['kind'] == 'Deployment')['spec']['template']['spec']
        require(pod.get('nodeSelector') == expected['nodeSelector'] and
                pod.get('tolerations') == expected['tolerations'] and
                len(pod['containers']) == 1 and pod['containers'][0]['image'] == current['image'] and
                resource_budget(pod['containers'][0]['resources']) == resource_budget(expected['containers'][0]['resources']) and
                all(pod['containers'][0].get(k) == expected['containers'][0].get(k)
                    for k in ('command', 'args', 'imagePullPolicy', 'securityContext')),
                'kueue_configuration_changed', 'controller image, placement or budget changed')
        cm = self.get('configmap', 'kueue-manager-config', SYSTEM_NS)
        expected_cm = next(v for v in configuration if v['kind'] == 'ConfigMap')
        require(cm['data'] == expected_cm['data'], 'kueue_configuration_changed', 'controller integration configuration changed')
        for kind in ('MutatingWebhookConfiguration', 'ValidatingWebhookConfiguration'):
            expected_hooks = next(v for v in configuration if v['kind'] == kind)['webhooks']
            actual = self.get(kind, next(v['metadata']['name'] for v in resources if v['kind'] == kind))
            actual_hooks = actual.get('webhooks', [])
            require([h['name'] for h in actual_hooks] == [h['name'] for h in expected_hooks],
                    'kueue_configuration_changed', 'webhook set changed')
            for actual_hook, expected_hook in zip(actual_hooks, expected_hooks):
                require(webhook_contract(actual_hook) == webhook_contract(expected_hook) and
                        bool(actual_hook['clientConfig'].get('caBundle')),
                        'kueue_configuration_changed', 'webhook scope or serving configuration changed')
        return {'version': self.lock['version'], 'image': current['image'], 'resource_count': len(current['uids'])}

    def install(self, path, supplied_image=None):
        upstream = checked_manifest(path, self.lock)  # Reject before any cluster access.
        image = self.image_source(supplied_image)
        with self.lab.operation_lock():
            self.attach()
            resources = objects(render_controller(self.lab.config, upstream, self.state['session'], image))
            previous = self.state.get('kueue')
            if previous:
                require(previous['definition'] == self.definition() and previous['image'] == image,
                        'kueue_definition_changed', 'refusing to replace an installed controller/version/source')
                if previous['phase'] == 'ready':
                    return dict(self.verify(), already_installed=True)
            found = self.inventory(resources)
            if previous:
                require(all(found.get(k) == v for k, v in previous.get('uids', {}).items()),
                        'kueue_identity_changed', 'partial installation resources were replaced')
            else:
                require(not found, 'unrecorded_kueue_installation', 'refusing to adopt an existing installation')
            references = [{'apiVersion': v['apiVersion'], 'kind': v['kind'],
                           'metadata': {k: v['metadata'][k] for k in ('name', 'namespace') if k in v['metadata']}}
                          for v in resources]
            current = {'phase': 'installing', 'definition': self.definition(), 'image': image,
                       'resources': references, 'uids': found,
                       'configuration': [v for v in resources if v['kind'] in
                           ('Deployment', 'ConfigMap', 'MutatingWebhookConfiguration', 'ValidatingWebhookConfiguration')]}
            self.state['kueue'] = current
            self.lab.write_state(self.state)
            self.save('kueue-rendered.json', {'apiVersion': 'v1', 'kind': 'List', 'items': resources})
            try:
                self.prepare_image(image)
                self.lab.kubectl('apply', '--server-side', '--field-manager=agentbox-platform-batch', '-f', '-',
                    input_text=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': resources}), timeout=180)
                current['uids'] = self.inventory(resources)
                require(len(current['uids']) == len(resources), 'kueue_install_incomplete', 'not all fixed resources exist')
                self.lab.write_state(self.state)
                self.lab.kubectl('-n', SYSTEM_NS, 'rollout', 'status', 'deployment/' + CONTROLLER,
                                 '--timeout=240s', timeout=260)
                current['phase'] = 'ready'
                result = self.verify()
                self.save('kueue-install.json', result)
                return result
            except BaseException:
                current['phase'] = 'failed'
                # Capture metadata only even after a partially successful server apply.
                current['uids'] = self.inventory(resources)
                raise
            finally:
                self.lab.write_state(self.state)


