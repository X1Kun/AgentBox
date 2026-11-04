"""Pinned Kueue installation and UID-fenced batch acceptance in one owned lab."""
import copy
from decimal import Decimal
import hashlib
import json
from pathlib import Path
import re
import time
import uuid

import yaml

from lib.platform_gvisor import GVisor, digest
from lib.platform_manifests import (UniqueLoader, canonical, load_lock, normalized,
    objects, read_yaml, render, render_upstream, repository_image_source)
from lib.platform_network import Network
from lib.platform_sessions import Sessions, checked_source

OWNER = 'agentbox.dev/session'
RUN = 'agentbox.dev/batch-run'
MANAGED = 'agentbox.dev/batch-managed'
SYSTEM_NS = 'kueue-system'
CONTROLLER = 'kueue-controller-manager'
API = 'kueue.x-k8s.io/v1beta2'


class BatchError(Exception):
    def __init__(self, reason, detail):
        super().__init__(detail)
        self.reason = reason


def require(value, reason, detail):
    if not value:
        raise BatchError(reason, detail)


def owner_reference(obj, uid, kind=None):
    return any(r.get('uid') == uid and r.get('controller') is True and
               (kind is None or r.get('kind') == kind) for r in obj['metadata'].get('ownerReferences', []))


def condition(obj, name, status='True'):
    return obj is not None and any(c['type'] == name and c['status'] == status for c in obj.get('status', {}).get('conditions', []))


def identity(obj, uid, owner, run=None):
    meta = obj['metadata']
    require(meta.get('uid') == uid and meta.get('labels', {}).get(OWNER) == owner and
            (run is None or meta['labels'].get(RUN) == run),
            'batch_identity_changed', meta['name'])


def quantity(value):
    """Exact CPU/memory quantity arithmetic for budget checks, never float rounding."""
    match = re.fullmatch(r'([0-9]+(?:\.[0-9]+)?)([numkKMGTPE]i?|)', str(value))
    require(match is not None, 'invalid_batch_quantity', str(value))
    number, suffix = match.groups()
    scales = {'': 1, 'n': Decimal('0.000000001'), 'u': Decimal('0.000001'), 'm': Decimal('.001'),
              'k': 1000, **{x: 1000 ** i for i, x in enumerate('KMGTPE', 1)},
              **{x + 'i': 1024 ** i for i, x in enumerate('KMGTPE', 1)}}
    require(suffix in scales, 'invalid_batch_quantity', str(value))
    return Decimal(number) * scales[suffix]


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


def validate_cleanup_record(value):
    run = value.get('run', '')
    require(isinstance(run, str) and re.fullmatch(r'[a-f0-9]{10}', run) and value.get('schema_version') == 1,
            'invalid_batch_cleanup', 'not a batch acceptance record')
    namespace, queue, flavor = 'agentbox-batch-' + run, 'agentbox-batch-' + run, 'agentbox-batch-worker-' + run
    require((value.get('namespace'), value.get('cluster_queue'), value.get('flavor')) == (namespace, queue, flavor) and
            isinstance(value.get('resources'), list) and isinstance(value.get('jobs'), dict),
            'invalid_batch_cleanup', 'batch record scope differs')
    expected = {'Namespace': (namespace, None), 'ClusterQueue': (queue, None), 'ResourceFlavor': (flavor, None),
                'LocalQueue': ('batch', namespace), 'ResourceQuota': ('batch-budget', namespace),
                'NetworkPolicy': ('batch-default-deny', namespace)}
    valid_uid = lambda uid: isinstance(uid, str) and re.fullmatch(r'[A-Za-z0-9-]{1,128}', uid)
    seen = set()
    for obj in value['resources']:
        kind = obj.get('kind')
        require(kind in expected and kind not in seen and (obj.get('name'), obj.get('namespace')) == expected[kind] and
                valid_uid(obj.get('uid')), 'invalid_batch_cleanup', 'unexpected fixture identity in batch report')
        seen.add(kind)
    for name, job in value['jobs'].items():
        require(isinstance(name, str) and re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', name) and
                job.get('name') == name and valid_uid(job.get('uid')), 'invalid_batch_cleanup', 'invalid Job identity')
        meta = job.get('manifest', {}).get('metadata', {})
        require(meta.get('name') == name and meta.get('namespace') == namespace and
                meta.get('labels', {}).get(OWNER) == value.get('owner') and meta['labels'].get(RUN) == run,
                'invalid_batch_cleanup', 'Job manifest belongs to another scope')


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


class BatchAcceptance:
    def __init__(self, lab):
        self.lab = lab
        self.kueue = Kueue(lab)
        self.runtime = None
        self.state = None
        self.record = None

    def save(self):
        self.lab.write_json(self.lab.runner.output / 'batch-test.json', self.record)

    def get(self, kind, name, namespace=None):
        return self.kueue.get(kind, name, namespace)

    def wait(self, predicate, detail, timeout=90):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            value = predicate()
            if value:
                return value
            time.sleep(.5)
        raise BatchError('batch_observation_timeout', detail)

    def params(self):
        return {'OWNER': self.state['session'], 'RUN': self.record['run'], 'NAME': self.record['namespace'],
                'NAMESPACE': self.record['namespace'], 'QUEUE_NAME': self.record['cluster_queue'],
                'FLAVOR_NAME': self.record['flavor']}

    def budget(self, resources):
        batch = next(o for o in resources if o['kind'] == 'ResourceQuota')['spec']['hard']
        queue = next(o for o in resources if o['kind'] == 'ClusterQueue')['spec']
        require(not queue.get('cohortName') and not queue.get('admissionChecksStrategy') and
                queue['preemption']['withinClusterQueue'] == 'Never' and
                queue['preemption']['reclaimWithinCohort'] == 'Never' and
                queue['preemption']['borrowWithinCohort']['policy'] == 'Never',
                'invalid_batch_budget', 'P3a requires an isolated static queue without borrowing or preemption')
        nominal = {v['name']: quantity(v['nominalQuota']) for v in queue['resourceGroups'][0]['flavors'][0]['resources']}
        job = render(self.lab.config, 'batch-job')['spec']['template']['spec']['containers'][0]['resources']
        for key in ('cpu', 'memory'):
            require(nominal[key] == quantity(batch['requests.' + key]) == quantity(batch['limits.' + key]) ==
                    quantity(job['requests'][key]) == quantity(job['limits'][key]),
                    'invalid_batch_budget', 'queue, namespace and Job budgets must agree: ' + key)
        session = read_yaml(self.lab.config / 'agentbox/overlays/lab/session/quota.yaml')['spec']['hard']
        nodes = json.loads(self.lab.kubectl('get', 'nodes', '-o', 'json', announce=False))['items']
        worker = next(v for v in nodes if v['metadata']['name'] == self.runtime.worker_name)
        pods = json.loads(self.lab.kubectl('get', 'pods', '-A', '--field-selector=spec.nodeName=' + self.runtime.worker_name,
                                        '-o', 'json', announce=False))['items']
        allocated = {key: Decimal(0) for key in ('cpu', 'memory')}
        for pod in pods:
            if pod.get('status', {}).get('phase') in ('Succeeded', 'Failed'):
                continue
            spec = pod['spec']
            for key in allocated:
                app = sum((quantity(c.get('resources', {}).get('requests', {}).get(key, '0')) for c in spec['containers']), Decimal(0))
                # Conservative for init/sidecar accounting: summing all requests overestimates reservation.
                app += sum((quantity(c.get('resources', {}).get('requests', {}).get(key, '0')) for c in spec.get('initContainers', [])), Decimal(0))
                allocated[key] += app + quantity(spec.get('overhead', {}).get(key, '0'))
        for key in allocated:
            require(allocated[key] + quantity(session['limits.' + key]) + nominal[key] <= quantity(worker['status']['allocatable'][key]),
                    'batch_capacity_insufficient', 'existing reservations plus session/batch limits exceed worker ' + key)
        self.record['budget'] = {'batch_quota': batch, 'session_budget_reference': session,
            'worker_allocatable': worker['status']['allocatable'], 'existing_pod_requests': {k: str(v) for k, v in allocated.items()},
            'scope': 'Conservative node scheduling check, not a physical reservation or an SLO guarantee; kind nodes share the host'}
        self.save()

    def create_resources(self):
        values = objects(render(self.lab.config, 'batch-resources', self.params()))
        self.budget(values)
        # Explicit order: namespace first, then quota/policy, then flavor and queues.
        order = {'Namespace': 0, 'ResourceQuota': 1, 'NetworkPolicy': 2, 'ResourceFlavor': 3, 'ClusterQueue': 4, 'LocalQueue': 5}
        for obj in sorted(values, key=lambda o: order[o['kind']]):
            created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
            self.record['resources'].append({'apiVersion': obj['apiVersion'], 'kind': obj['kind'],
                'name': obj['metadata']['name'], 'namespace': obj['metadata'].get('namespace'), 'uid': created['metadata']['uid']})
            self.save()
        self.wait(lambda: condition(self.get('clusterqueue', self.record['cluster_queue']), 'Active'), 'queue activation')
        self.wait(lambda: condition(self.get('localqueue', 'batch', self.record['namespace']), 'Active'), 'local queue activation')

    def create_job(self, name, mode='success', profile='batch-job'):
        params = dict(self.params(), NAME=name, IMAGE=self.record['image'])
        obj = render(self.lab.config, profile, params)
        obj['spec']['template']['spec']['containers'][0]['command'] = ['/usr/local/bin/python3', '-I', '-B', '-u', '-c',
            (self.lab.root / 'scripts/lib/batch_guest_probe.py').read_text(), mode]
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        record = {'name': name, 'uid': created['metadata']['uid'], 'mode': mode, 'profile': profile,
                  'manifest': obj, 'workload': None, 'pod': None, 'cleanup_complete': False}
        self.record['jobs'][name] = record
        self.save()
        return record

    def job(self, record):
        obj = self.get('job', record['name'], self.record['namespace'])
        require(obj is not None, 'batch_job_missing', record['name'])
        identity(obj, record['uid'], self.state['session'], self.record['run'])
        require(obj['metadata']['labels'].get('kueue.x-k8s.io/queue-name') == 'batch', 'batch_identity_changed', 'Job queue changed')
        return obj

    def workloads(self, record):
        values = json.loads(self.lab.kubectl('-n', self.record['namespace'], 'get', 'workloads.kueue.x-k8s.io',
                                           '-o', 'json', announce=False))['items']
        return [v for v in values if owner_reference(v, record['uid'], 'Job')]

    def workload(self, record):
        values = self.workloads(record)
        require(len(values) <= 1, 'duplicate_batch_workload', record['name'])
        if not values:
            return None
        obj = values[0]
        require(obj['spec']['queueName'] == 'batch', 'batch_identity_changed', 'Workload queue changed')
        previous = record['workload']
        if previous:
            require(obj['metadata']['uid'] == previous['uid'], 'batch_identity_changed', 'Workload replaced')
        else:
            record['workload'] = {'name': obj['metadata']['name'], 'uid': obj['metadata']['uid']}
            self.save()
        return obj

    def pods(self, record):
        values = json.loads(self.lab.kubectl('-n', self.record['namespace'], 'get', 'pods', '-l',
            'batch.kubernetes.io/job-name=' + record['name'], '-o', 'json', announce=False))['items']
        for pod in values:
            meta, spec = pod['metadata'], pod['spec']
            require(owner_reference(pod, record['uid'], 'Job') and meta.get('labels', {}).get(OWNER) == self.state['session'] and
                    meta['labels'].get(RUN) == self.record['run'], 'batch_identity_changed', 'Pod owner/run differs')
            require(spec.get('runtimeClassName') == 'agentbox-gvisor' and spec.get('nodeName') in (None, self.runtime.worker_name) and
                    spec.get('automountServiceAccountToken') is False and len(spec['containers']) == 1 and
                    spec['containers'][0]['image'] == self.record['image'] and
                    spec['containers'][0]['resources'] == record['manifest']['spec']['template']['spec']['containers'][0]['resources'],
                    'batch_identity_changed', 'Pod runtime, placement, image, credentials or budget differs')
            if record['pod']:
                require(meta['uid'] == record['pod']['uid'], 'batch_identity_changed', 'Pod attempt replaced')
        require(len(values) <= 1, 'unexpected_batch_attempt', 'backoffLimit=0 must produce at most one Pod')
        return values

    def logs(self, pod):
        return self.lab.kubectl('-n', self.record['namespace'], 'logs', pod['metadata']['name'], '-c', 'compute',
                                '--limit-bytes=8192', announce=False)

    def pending(self, record):
        samples = []
        until = time.monotonic() + 60
        while time.monotonic() < until:
            job, workload, pods = self.job(record), self.workload(record), self.pods(record)
            require(not pods and job['spec']['suspend'] is True and not (workload and condition(workload, 'Admitted')),
                    'unexpected_batch_admission', 'quota-blocked Job started or created a Pod')
            if workload and workload.get('status', {}).get('admission') is None and condition(workload, 'QuotaReserved', 'False'):
                samples.append({'at': time.time(), 'job_uid': job['metadata']['uid'], 'workload_uid': workload['metadata']['uid'],
                                'conditions': workload['status'].get('conditions', []), 'pods': 0})
                if len(samples) >= 2:
                    record['pending_samples'] = samples
                    self.save()
                    return
            time.sleep(2)
        raise BatchError('batch_pending_not_confirmed', record['name'])

    def running(self, record):
        def observe():
            job, workload = self.job(record), self.workload(record)
            values = self.pods(record)
            if not values or not workload or not condition(workload, 'Admitted'):
                return None
            pod = values[0]
            require(not condition(job, 'Failed') and pod.get('status', {}).get('phase') not in ('Failed', 'Succeeded'),
                    'batch_ended_early', record['name'])
            if pod['status'].get('phase') != 'Running' or not condition(pod, 'Ready'):
                return None
            require(workload['status']['admission']['clusterQueue'] == self.record['cluster_queue'] and
                    job['spec']['suspend'] is False and pod['spec']['nodeName'] == self.runtime.worker_name,
                    'batch_identity_changed', 'wrong admission, suspension or execution node')
            text = self.logs(pod)
            if not text.strip():
                return None
            try:
                ready = json.loads(text.splitlines()[0])
            except ValueError as error:
                raise BatchError('invalid_batch_output', text[:1000]) from error
            require(all(ready.get(k) == v for k, v in {'phase': 'running', 'run': self.record['run'], 'pod_uid': pod['metadata']['uid'],
                    'mode': record['mode'], 'uid': 1000, 'readonly_root': True, 'serviceaccount_token': False}.items()),
                    'invalid_batch_output', 'running marker does not match this Pod/run/security contract')
            if record['mode'] == 'cancel':
                require(isinstance(ready.get('child_pid'), int) and ready['child_pid'] > 1,
                        'invalid_batch_output', 'cancellation must include a running child')
            record['pod'] = {'name': pod['metadata']['name'], 'uid': pod['metadata']['uid']}
            record['ready'] = ready
            record['admission'] = copy.deepcopy(workload['status']['admission'])
            self.save()
            return pod
        pod = self.wait(observe, 'admission and real batch startup', timeout=120)
        record['runtime'] = self.runtime.runtime_evidence(pod)
        self.save()
        if record['mode'] in ('success', 'failure'):
            self.activate(record, pod)
        return pod

    def activate(self, record, pod):
        try:
            self.lab.kubectl('-n', self.record['namespace'], 'exec', pod['metadata']['name'], '-c', 'compute', '--',
                '/usr/local/bin/python3', '-I', '-B', '-c', "from pathlib import Path; Path('/work/go').touch()")
        except Exception as error:
            # PID 1 can finish after the marker is written but before exec reports
            # success, killing that auxiliary process. Never replay the marker.
            # The caller must still prove this same Job/Pod's terminal result and cleanup.
            commands = self.lab.runner.commands
            code = commands[-1].get('exit_code') if commands else None
            if getattr(error, 'reason', None) != 'command_failed' or code != 137:
                raise
            record['activation_transport_error'] = {'exit_code': code, 'detail': str(error)}
            self.save()

    def terminal(self, record):
        def observe():
            job = self.job(record)
            if record['mode'] == 'deadline':
                failure = next((c for c in job.get('status', {}).get('conditions', [])
                    if c['type'] == 'Failed' and c['status'] == 'True'), None)
                if failure:
                    require(failure.get('reason') == 'DeadlineExceeded', 'invalid_batch_terminal', 'expected Job deadline')
                    record['job_status'] = job['status']
                    record['terminal_evidence'] = 'DeadlineExceeded; no invented container exit code'
                    return job
                return None
            if not (condition(job, 'Complete') or condition(job, 'Failed')):
                return None
            pods = self.pods(record)
            require(len(pods) == 1 and record['pod'] is not None, 'invalid_batch_terminal', 'missing observed Pod')
            term = pods[0]['status'].get('containerStatuses', [{}])[0].get('state', {}).get('terminated', {})
            text = self.logs(pods[0])
            last = json.loads(text.splitlines()[-1])
            require(last.get('run') == self.record['run'] and last.get('pod_uid') == record['pod']['uid'],
                    'invalid_batch_output', 'result belongs to another attempt/run')
            if record['mode'] == 'failure':
                require(condition(job, 'Failed') and term.get('exitCode') == 23 and last.get('phase') == 'failed',
                        'invalid_batch_terminal', 'expected explicit exit 23 without retry')
            else:
                require(condition(job, 'Complete') and term.get('exitCode') == 0 and
                        last.get('phase') == 'complete' and last.get('result') == 333833500 and last.get('iterations', 0) > 0,
                        'invalid_batch_terminal', 'Job completion does not prove the result correct')
            record.update(job_status=job['status'], termination=term, logs=text)
            self.save()
            return job
        return self.wait(observe, 'batch terminal/result evidence', timeout=120)

    def delete(self, kind, name, uid, namespace=None):
        path = '/apis/' + API + '/' + ('namespaces/' + namespace + '/' if namespace else '')
        resource = {'ClusterQueue': 'clusterqueues', 'ResourceFlavor': 'resourceflavors', 'LocalQueue': 'localqueues'}.get(kind)
        if kind == 'Job':
            path, resource = '/apis/batch/v1/namespaces/' + namespace + '/', 'jobs'
        if kind == 'Namespace':
            path, resource = '/api/v1/', 'namespaces'
        require(resource is not None, 'invalid_batch_cleanup', kind)
        options = {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'propagationPolicy': 'Foreground', 'preconditions': {'uid': uid}}
        self.lab.kubectl('delete', '--raw', path + resource + '/' + name, '-f', '-', input_text=json.dumps(options), announce=False)

    def cleanup_job(self, record):
        job = self.get('job', record['name'], self.record['namespace'])
        if job is not None:
            identity(job, record['uid'], self.state['session'], self.record['run'])
            pods = self.pods(record)
            # Capture even a not-yet-ready Pod before deleting it; cleanup is independent of successful execution.
            if pods and record['pod'] is None:
                record['pod'] = {'name': pods[0]['metadata']['name'], 'uid': pods[0]['metadata']['uid']}
                self.save()
            # Snapshot CRI identities before deletion, including a failed startup
            # that never reached runtime_evidence. Orphan processes can outlive CRI entries.
            uid = record['pod']['uid'] if record.get('pod') else None
            sandboxes = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
            captured = [s['id'] for s in sandboxes if uid and s.get('labels', {}).get('io.kubernetes.pod.uid') == uid]
            require(all(re.fullmatch(r'[a-f0-9]{64}', sid) for sid in captured), 'invalid_batch_cleanup', 'invalid sandbox ID')
            record['cleanup_sandbox_ids'] = sorted(set(record.get('cleanup_sandbox_ids', [])) | set(captured))
            self.save()
            self.delete('Job', record['name'], record['uid'], self.record['namespace'])
        uid = record.get('pod', {}).get('uid') if record.get('pod') else None
        def reclaimed():
            current = self.get('job', record['name'], self.record['namespace'])
            if current:
                identity(current, record['uid'], self.state['session'], self.record['run'])
            pods = self.pods(record)
            workloads = self.workloads(record)
            sandboxes = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
            sandboxes = [s for s in sandboxes if uid and s.get('labels', {}).get('io.kubernetes.pod.uid') == uid]
            sandbox_ids = set(record.get('cleanup_sandbox_ids', [])) | {s['id'] for s in sandboxes}
            if record.get('runtime'):
                sandbox_ids.add(record['runtime']['sandbox_id'])
            require(all(re.fullmatch(r'[a-f0-9]{64}', sid) for sid in sandbox_ids), 'invalid_batch_cleanup', 'invalid sandbox ID')
            record['cleanup_sandbox_ids'] = sorted(sandbox_ids)
            live = {sid: self.runtime.processes(sid) for sid in sorted(sandbox_ids)}
            self.save()
            return not current and not pods and not workloads and not sandboxes and not any(live.values())
        self.wait(reclaimed, 'Job/Workload/Pod/CRI/process reclamation', timeout=90)
        record['cleanup_complete'] = True
        self.save()

    def cleanup_resources(self):
        namespace = next((r for r in self.record['resources'] if r['kind'] == 'Namespace'), None)
        if namespace:
            actual = self.get('Namespace', namespace['name'])
            if actual is not None:
                identity(actual, namespace['uid'], self.state['session'], self.record['run'])
        for record in self.record['jobs'].values():
            self.cleanup_job(record)
        resources = self.record['resources']
        for kind in ('LocalQueue', 'ClusterQueue', 'ResourceFlavor', 'Namespace'):
            for obj in [r for r in resources if r['kind'] == kind]:
                actual = self.get(kind, obj['name'], obj.get('namespace'))
                if actual is None:
                    continue
                identity(actual, obj['uid'], self.state['session'], self.record['run'])
                self.delete(kind, obj['name'], obj['uid'], obj.get('namespace'))
                self.wait(lambda: self.get(kind, obj['name'], obj.get('namespace')) is None, 'batch fixture deletion')
        self.record['cleanup_complete'] = True
        self.save()

    def cleanup(self, report):
        value = json.loads(Path(report).read_text())
        validate_cleanup_record(value)
        with self.lab.operation_lock():
            self.runtime = self.kueue.attach()
            self.state = self.kueue.state
            require(value.get('owner') == self.state['session'], 'unowned_batch_report', 'report belongs to another cluster session')
            self.record = value
            self.cleanup_resources()
            return {'run': value['run'], 'cleanup_complete': True}

    def test(self, image):
        with self.lab.operation_lock():
            self.runtime = self.kueue.attach()
            self.state = self.kueue.state
            self.kueue.verify()
            # Prepare the worker's exact digest before starting any Job observation budget.
            network = Network(self.lab)
            network.state = dict(self.state, node_ids=[self.runtime.worker])
            network.prepare_images({'batch': image})
            run = uuid.uuid4().hex[:10]
            self.record = {'schema_version': 1, 'owner': self.state['session'], 'run': run, 'image': image,
                'namespace': 'agentbox-batch-' + run, 'cluster_queue': 'agentbox-batch-' + run,
                'flavor': 'agentbox-batch-worker-' + run, 'resources': [], 'jobs': {}, 'checks': [],
                'cleanup_complete': False, 'all_passed': False}
            self.save()
            try:
                self.create_resources()
                holder = self.create_job('holder', 'cancel')
                self.running(holder)
                print('[platform] batch: quota occupied by real parent/child computation', flush=True)
                queued = self.create_job('queued-cancel')
                self.pending(queued)
                self.cleanup_job(queued)
                self.record['checks'].append({'name': 'quota_wait_and_queued_cancel_without_pod', 'passed': True})
                resumed = self.create_job('resume-after-release')
                self.pending(resumed)
                self.cleanup_job(holder)
                self.record['checks'].append({'name': 'running_parent_child_cancel_and_runtime_reclamation', 'passed': True})
                self.running(resumed)
                self.terminal(resumed)
                self.cleanup_job(resumed)
                self.record['checks'].append({'name': 'released_quota_admission_real_gvisor_result_and_cleanup', 'passed': True})
                oversized = self.create_job('oversized', profile='batch-oversized')
                self.pending(oversized)
                self.cleanup_job(oversized)
                self.record['checks'].append({'name': 'oversized_request_not_admitted', 'passed': True})
                failed = self.create_job('explicit-failure', 'failure')
                self.running(failed)
                self.terminal(failed)
                self.cleanup_job(failed)
                self.record['checks'].append({'name': 'explicit_execution_failure_without_retry', 'passed': True})
                deadline = self.create_job('deadline', 'deadline', 'batch-deadline')
                self.running(deadline)
                self.terminal(deadline)
                self.cleanup_job(deadline)
                self.record['checks'].append({'name': 'job_deadline_and_runtime_reclamation', 'passed': True})
                self.cleanup_resources()
                self.kueue.verify()
                self.record.update(all_passed=True, development_cluster_retained=True)
                return self.record
            finally:
                self.save()
