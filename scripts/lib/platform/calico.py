"""Install and verify Calico in an owned kind lab."""

from lib.core.paths import ROOT as PROJECT_ROOT
from lib.core.errors import NetworkError
from lib.manifests import load_lock, definition as manifest_definition, lock_identity, render_upstream
import json
from pathlib import Path
import re
import time

from lib.platform.gvisor import digest

OWNER = 'agentbox.dev/session'
ROLE = 'agentbox.dev/net-role'


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


def render_calico(objects, lock, session, prefix=None, config=None):
    if len(objects) != lock['object_count']:
        raise NetworkError('unexpected_calico_manifest', 'upstream object count changed')
    sources = image_sources(lock, prefix)
    seen = set()
    for obj in objects:
        if obj['kind'] == 'ConfigMap' and obj['metadata']['name'] == 'calico-config' and obj['data']['calico_backend'] != 'vxlan':
            raise NetworkError('unexpected_calico_backend', 'use the official VXLAN manifest')
        if obj['kind'] not in ('DaemonSet','Deployment'): continue
        spec = obj['spec']['template']['spec']
        for container in spec.get('initContainers', []) + spec['containers']:
            if container['image'] not in sources:
                raise NetworkError('unexpected_calico_image', container['image'])
            seen.add(container['image'])
    if seen != set(sources):
        raise NetworkError('unexpected_calico_images', 'not all locked image components were present')
    images = [{'name': original.rsplit(':',1)[0], 'newName': value.split('@')[0], 'digest': value.split('@')[1]} for original,value in sources.items()]
    from lib.manifests import objects as unpack
    rendered = render_upstream(config or PROJECT_ROOT/'deploy',
        'calico-lab', objects, {'OWNER':session, 'POD_CIDR':lock['pod_cidr']}, images)
    return {'apiVersion':'v1','kind':'List','items':unpack(rendered)}


class Network:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.lock = load_lock(lab.config, 'calico')
        if (self.lock.get('schema_version') != 1 or self.lock.get('mode') != 'VXLAN' or
                not re.fullmatch('[a-f0-9]{64}', self.lock.get('sha256', ''))):
            raise NetworkError('invalid_calico_lock', 'unsupported Calico definition')
        self.state = None

    def definition(self):
        return manifest_definition(self.lab.config, 'network')

    def attach(self, ready=False):
        self.state, actual = self.lab.owned(complete=ready)
        if (self.state.get('network_profile') != 'calico' or len(actual) != 2 or
                actual != self.state.get('node_ids') or self.state['phase'] not in ('network_pending', 'ready')):
            raise NetworkError('wrong_network_profile', 'requires a fresh lab created with --network-profile calico')
        if self.state.get('network_definition') != self.definition():
            raise NetworkError('network_definition_changed', 'recreate the lab before changing the CNI definition')
        if lock_identity(self.lab.config, 'versions') != self.state['lock_sha256']:
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
            rendered = render_calico(parsed, self.lock, self.state['session'], prefix, self.lab.config)
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


