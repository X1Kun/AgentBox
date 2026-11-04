"""Fixed image worker builds and the real batch product acceptance fixture."""
import hashlib
import base64
import http.client
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import urllib.parse
import uuid

from PIL import Image
import yaml

from lib.platform_batch import BatchError, BatchAcceptance, Kueue, require, identity, quantity, OWNER, RUN
from lib.platform_api_execution import APIExecutionAcceptance
from lib.platform_sessions import Sessions
from lib.platform_manifests import canonical, objects, render, read_yaml
from lib.api_network import add_api_egress, discover_api_endpoints
from lib.product_images import bind_digest
from prepare_batch_images import prepare


def docker_image(lab, name):
    return json.loads(lab.runner.run([lab.tools['docker'], 'image', 'inspect', name], announce=False))[0]


def import_image(lab, image, image_id, node, archive):
    def cached():
        values = json.loads(lab.runner.run([lab.tools['docker'], 'exec', node, 'crictl', 'images', '-o', 'json'], announce=False))['images']
        return any(v['id'] == image_id and 'docker.io/' + image in v.get('repoTags', []) for v in values)
    if cached():
        return True
    processes = lab.runner.run([lab.tools['docker'], 'exec', node, 'ps', '-eo', 'comm='], announce=False)
    require('ctr' not in processes.split(), 'batch_image_import_busy', 'an existing node image import must finish first')
    lab.runner.run([lab.tools['docker'], 'save', '-o', archive, image], timeout=300)
    lab.runner.run([lab.tools['kind'], 'load', 'image-archive', archive, '--name', 'agentbox-platform-lab', '--nodes', node], timeout=300)
    until = time.monotonic() + 60
    while time.monotonic() < until:
        if cached():
            return False
        time.sleep(.5)
    raise BatchError('batch_image_not_imported', 'node did not confirm the expected image ID and tag')


def build_worker(lab):
    cache, contract = prepare()
    with lab.operation_lock():
        state, _ = lab.owned(complete=True)
        base = lab.python_image()
        lab.prepare_image(base)
        original = docker_image(lab, base)
        require(base in original.get('RepoDigests', []), 'batch_base_not_confirmed', 'base digest not confirmed')
        files = ['deploy/agentbox/batch-images/dataset.yaml', 'deploy/agentbox/batch-images/versions.lock.yaml',
                 'deploy/agentbox/batch-images/contract.yaml', 'deploy/agentbox/batch-images/worker.Dockerfile',
                 'scripts/lib/image_worker.py']
        inputs = {name: hashlib.sha256((lab.root / name).read_bytes()).hexdigest() for name in files}
        inputs.update(base=base, base_id=original['Id'])
        fingerprint = canonical(inputs)
        image = 'agentbox/image-worker:' + fingerprint[:16]
        path = lab.state_dir / 'batch-image-bundle.json'
        previous = json.loads(path.read_text()) if path.exists() else None
        reused = bool(previous and previous['inputs'] == inputs)
        if reused:
            require(docker_image(lab, image)['Id'] == previous['image_id'], 'batch_image_changed', 'cached worker image ID differs')
            image_id = previous['image_id']
        else:
            with tempfile.TemporaryDirectory(prefix='batch-image-', dir=lab.state_dir) as tmp:
                context = Path(tmp)
                (context / 'images').mkdir()
                (context / 'wheels').mkdir()
                manifest = json.loads((cache / 'images.json').read_text())
                for item in manifest['images']:
                    shutil.copyfile(cache / item['name'], context / 'images' / item['name'])
                from lib.platform_manifests import read_yaml
                wheel = read_yaml(lab.config / 'agentbox/batch-images/versions.lock.yaml')['wheel']['filename']
                shutil.copyfile(cache / wheel, context / 'wheels' / wheel)
                shutil.copyfile(cache / 'images.json', context / 'images.json')
                shutil.copyfile(lab.root / 'scripts/lib/image_worker.py', context / 'worker.py')
                shutil.copyfile(lab.config / 'agentbox/batch-images/worker.Dockerfile', context / 'Dockerfile')
                lab.runner.run([lab.tools['docker'], 'build', '--pull=false', '--network=none', '--build-arg',
                                'PYTHON_IMAGE=' + base, '-t', image, context], timeout=300)
            image_id = docker_image(lab, image)['Id']
        reference = ("import runpy,json;from pathlib import Path;w=runpy.run_path('/opt/worker.py');"
            "m=json.loads(Path('/opt/images.json').read_text());"
            "print(json.dumps([{'name':n,'pixel_sha256':w['sha'](im.tobytes()),'width':im.width,'height':im.height} "
            "for n,im in w['outputs'](Path('/opt/images'),m)]))")
        computed = json.loads(lab.runner.run([lab.tools['docker'], 'run', '--rm', '--read-only', '--network=none',
            '--memory=256m', '--cpus=.5', '--entrypoint=python3', image, '-I', '-B', '-c', reference], timeout=60))
        require(computed == contract['outputs'], 'batch_pixel_contract_changed', 'target image codec output differs from the reviewed pixel contract')
        imported = import_image(lab, image, image_id, state['cluster'] + '-worker',
                                lab.state_dir / ('batch-image-' + fingerprint[:16] + '.tar'))
        inventory = json.loads(lab.runner.run([lab.tools['docker'], 'exec', state['cluster'] + '-worker',
            'crictl', 'images', '-o', 'json'], announce=False))['images']
        matches = [v for v in inventory if v['id'] == image_id and 'docker.io/' + image in v.get('repoTags', [])]
        require(len(matches) == 1, 'batch_image_identity_missing', 'CRI tag/config identity differs')
        descriptor = bind_digest(lab,state['cluster']+'-worker',image,image_id,create_alias=False)
        result = {'schema_version': 1, 'inputs': inputs, 'fingerprint': fingerprint, 'image': image,
                  'image_id': image_id, 'image_digest': descriptor['manifest_digest'], 'dataset': contract['dataset_id'], 'dataset_sha256': contract['dataset_sha256'],
                  'docker_id': state['docker_id'], 'reused': reused, 'node_cache_reused': imported}
        lab.write_json(path, result)
        lab.write_json(lab.runner.output / 'batch-image-build.json', result)
        return result


def worker_bundle(lab):
    value = json.loads((lab.state_dir / 'batch-image-bundle.json').read_text())
    for name, digest in value['inputs'].items():
        if name not in ('base', 'base_id'):
            require(hashlib.sha256((lab.root / name).read_bytes()).hexdigest() == digest,
                    'batch_image_inputs_changed', 'rebuild the image worker for current inputs')
    require(docker_image(lab, value['image'])['Id'] == value['image_id'], 'batch_image_changed', 'worker image was replaced')
    require(re.fullmatch('sha256:[a-f0-9]{64}', value.get('image_digest', '')) is not None,
            'batch_image_identity_missing', 'rerun batch-image-build to bind the CRI manifest digest')
    state, _ = lab.owned(complete=True)
    descriptor = bind_digest(lab,state['cluster']+'-worker',value['image'],value['image_id'],create_alias=False)
    if descriptor['manifest_digest'] != value['image_digest']:
        value['recorded_manifest_digest'] = value['image_digest']
        value['image_digest'] = descriptor['manifest_digest']
    return value


class ProductAcceptance(APIExecutionAcceptance):
    def __init__(self, lab):
        super().__init__(lab)
        self.kueue = Kueue(lab)
        self.runtime = None
        self.state = None
        self.http = None

    def save(self):
        self.lab.write_json(self.output / 'batch-product-test.json', self.record)

    def get(self, kind, name, namespace=None):
        return self.kueue.get(kind, name, namespace)

    def connection(self):
        if self.http is None:
            target = urllib.parse.urlsplit(self.base)
            self.http = http.client.HTTPSConnection(target.hostname, target.port, context=self.tls_context, timeout=10)
        return self.http

    def request(self, method, path, token, body=None):
        payload = json.dumps(body).encode() if body is not None else None
        until = time.monotonic() + 15
        for attempt in range(3):
            try:
                connection = self.connection()
                connection.timeout = max(.1, min(10, until - time.monotonic()))
                if connection.sock:
                    connection.sock.settimeout(connection.timeout)
                connection.request(method, path, payload, {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
                response = connection.getresponse()
                raw = response.read((1 << 20) + 1)
                self.require(len(raw) <= 1 << 20, 'batch response exceeds fixture byte budget')
                return response.status, raw
            except ssl.SSLCertVerificationError:
                raise
            except (ssl.SSLError, OSError, http.client.HTTPException) as error:
                allowed = method in ('GET', 'DELETE') or (method == 'POST' and path == '/v2/batches' and body and body.get('request_id'))
                if self.http:
                    self.http.close()
                self.http = None
                if not allowed or attempt == 2 or time.monotonic() >= until:
                    raise
                self.record.setdefault('transport_recoveries', []).append({'method': method, 'path': path, 'error': type(error).__name__})
                self.save()
                time.sleep(.1)

    def api(self, method, path, token, body=None, expected=None):
        status, raw = self.request(method, path, token, body)
        value = json.loads(raw)
        if expected is not None:
            self.require(status in expected, 'unexpected HTTP ' + str(status) + ' for ' + method + ' ' + path + ' (' + str(value.get('error', '')) + ')')
        return status, value

    def stop(self):
        if self.http:
            self.http.close()
            self.http = None
        super().stop()

    def create(self, obj):
        created = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))
        self.record['resources'].append({'kind': obj['kind'], 'name': obj['metadata']['name'],
            'namespace': obj['metadata'].get('namespace'), 'uid': created['metadata']['uid']})
        self.save()
        return created

    def connect(self, namespace, previous_uid=None):
        self.lab.kubectl('-n', namespace, 'rollout', 'status', 'deployment/agentbox-batch-api', '--timeout=180s', timeout=200)
        until = time.monotonic() + 180
        while time.monotonic() < until:
            pods = json.loads(self.lab.kubectl('-n', namespace, 'get', 'pods', '-l', 'app=agentbox-batch-api', '-o', 'json', announce=False))['items']
            pods = [p for p in pods if p['metadata']['uid'] != previous_uid and not p['metadata'].get('deletionTimestamp') and
                    any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', []))]
            if len(pods) == 1:
                break
            time.sleep(.5)
        self.require(len(pods) == 1, 'expected one ready batch API Pod')
        self.record['api_pod'] = {'name': pods[0]['metadata']['name'], 'uid': pods[0]['metadata']['uid']}
        self.save()
        self.log = (self.output / 'batch-api-port-forward.log').open('w')
        self.process = subprocess.Popen([self.lab.tools['kubectl'], '--kubeconfig', str(self.lab.kubeconfig),
            '--context', 'kind-agentbox-platform-lab', '-n', namespace, 'port-forward', '--address=127.0.0.1',
            'pod/' + pods[0]['metadata']['name'], ':8090'], stdin=subprocess.DEVNULL, stdout=self.log, stderr=self.log)
        until = time.monotonic() + 20
        while time.monotonic() < until:
            self.require(self.process.poll() is None, 'batch port-forward stopped')
            for line in (self.output / 'batch-api-port-forward.log').read_text().splitlines():
                if line.startswith('Forwarding from 127.0.0.1:'):
                    self.base = 'https://' + line.split(' ')[2]
                    status, _ = self.api('GET', '/readyz', '', expected={200, 503})
                    if status == 200:
                        return
            time.sleep(.2)
        raise BatchError('batch_api_connect_timeout', 'batch API was not ready')

    def policies(self, obj):
        add_api_egress(obj, discover_api_endpoints(self.lab.kubectl))

    def budget(self, values):
        deployment = next(v for v in values if v['kind'] == 'Deployment')
        api = deployment['spec']['template']['spec']['containers'][0]['resources']['limits']
        contract = yaml.safe_load(next(v for v in values if v['kind'] == 'ConfigMap')['data']['job.yaml'])
        worker = contract['spec']['template']['spec']['containers'][0]['resources']['limits']
        session = read_yaml(self.lab.config / 'agentbox/overlays/lab/session/quota.yaml')['spec']['hard']
        nodes = json.loads(self.lab.kubectl('get', 'nodes', '-o', 'json', announce=False))['items']
        node = next(n for n in nodes if n['metadata']['name'] == self.runtime.worker_name)
        pods = json.loads(self.lab.kubectl('get', 'pods', '-A', '--field-selector=spec.nodeName=' + self.runtime.worker_name,
            '-o', 'json', announce=False))['items']
        allocated = {key: quantity('0') for key in ('cpu', 'memory')}
        for pod in pods:
            if pod.get('status', {}).get('phase') in ('Succeeded', 'Failed'):
                continue
            for c in pod['spec']['containers'] + pod['spec'].get('initContainers', []):
                for key in allocated:
                    allocated[key] += quantity(c.get('resources', {}).get('requests', {}).get(key, '0'))
            for key in allocated:
                allocated[key] += quantity(pod['spec'].get('overhead', {}).get(key, '0'))
        for key, current in allocated.items():
            self.require(current + quantity(api[key]) + quantity(worker[key]) + quantity(session['limits.' + key]) <=
                quantity(node['status']['allocatable'][key]), 'insufficient worker capacity with full session/API/batch budgets')
        self.record['budget'] = {'session_reference': session, 'api_limits': api, 'worker_limits': worker,
            'worker_allocatable': node['status']['allocatable'], 'existing_requests': {k: str(v) for k, v in allocated.items()},
            'scope': 'Conservative scheduling check only; kind nodes still share physical host resources'}
        self.save()

    def capture(self, view, require_running=False, previous_uid=None):
        name, uid = view['batch_id'].split('.')
        job = self.get('job', name, self.record['namespace'])
        self.require(job is not None and job['metadata']['uid'] == uid, 'batch API Job identity differs')
        values = json.loads(self.lab.kubectl('-n', self.record['namespace'], 'get', 'pods', '-l',
            'batch.kubernetes.io/controller-uid=' + uid, '-o', 'json', announce=False))['items']
        record = self.record['jobs'].setdefault(name, {'name': name, 'uid': uid, 'pods': {}, 'manifest': job})
        self.save()
        for pod in values:
            self.require(any(r.get('uid') == uid and r.get('controller') for r in pod['metadata'].get('ownerReferences', [])), 'Pod owner differs')
            self.require(pod['spec'].get('automountServiceAccountToken') is False and pod['spec'].get('runtimeClassName') == 'agentbox-gvisor', 'worker credentials/runtime differ')
            pod_uid = pod['metadata']['uid']
            record['pods'].setdefault(pod_uid, {'name': pod['metadata']['name'], 'uid': pod_uid})
            if pod_uid == previous_uid:
                continue
            statuses = pod.get('status', {}).get('containerStatuses', [])
            if require_running and statuses and statuses[0].get('state', {}).get('running') and pod['status'].get('phase') == 'Running':
                runtime = self.runtime.runtime_evidence(pod)
                self.record['runtime_evidence'][pod_uid] = runtime
                record['pods'][pod_uid]['sandbox_id'] = runtime['sandbox_id']
                self.save()
                return pod
        self.save()
        return None

    def wait_running(self, view, previous_uid=None):
        until = time.monotonic() + 120
        while time.monotonic() < until:
            _, current = self.api('GET', '/v2/batches/' + view['batch_id'], self.alice, expected={200})
            self.require(current['state'] not in ('succeeded', 'failed', 'cancelled', 'unknown'), 'batch ended before runtime evidence: ' + str(current))
            pod = self.capture(view, True, previous_uid)
            if pod:
                return pod
            time.sleep(.5)
        raise BatchError('batch_product_start_timeout', 'no real running image worker')

    def wait_task(self, view, state, seconds=180, cleanup=False):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            _, current = self.api('GET', '/v2/batches/' + view['batch_id'], self.alice, expected={200})
            if current['state'] == state and (not cleanup or current['cleanup_complete']):
                self.record['views'][view['batch_id']] = current
                self.save()
                return current
            self.require(current['state'] not in ('failed', 'unknown') or current['state'] == state, 'unexpected batch state: ' + str(current))
            time.sleep(.5)
        raise BatchError('batch_product_state_timeout', 'did not observe ' + state)

    def download(self, view, name, token=None, expected=200):
        status, data = self.request('GET', '/v2/batches/' + view['batch_id'] + '/artifacts/' + name, token or self.alice)
        self.require(status == expected, 'download status differs: ' + str(status))
        self.require(len(data) <= 256 << 10, 'artifact download exceeds file budget')
        return data

    def verify_artifacts(self, view):
        contract = read_yaml(self.lab.config / 'agentbox/batch-images/contract.yaml')
        expected = {v['name']: v for v in contract['outputs']}
        self.require(len(view['artifacts']) == len(expected), 'artifact count differs')
        downloaded = {}
        for item in view['artifacts']:
            raw = self.download(view, item['name'])
            self.require(hashlib.sha256(raw).hexdigest() == item['sha256'] and len(raw) == item['bytes'], 'download byte checksum differs')
            with Image.open(io.BytesIO(raw)) as image:
                self.require(image.size == (expected[item['name']]['width'], expected[item['name']]['height']) and
                    hashlib.sha256(image.convert('RGB').tobytes()).hexdigest() == expected[item['name']]['pixel_sha256'], 'downloaded pixels differ')
            downloaded[item['name']] = raw
        return downloaded

    def wait_runtime_cleanup(self, name):
        record = self.record['jobs'][name]
        until = time.monotonic() + 90
        while time.monotonic() < until:
            cri = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
            remaining = [v for v in cri if v.get('labels', {}).get('io.kubernetes.pod.uid') in record['pods']]
            processes = [p for uid in record['pods'] if uid in self.record['runtime_evidence']
                         for p in self.runtime.processes(self.record['runtime_evidence'][uid]['sandbox_id'])]
            if not remaining and not processes:
                record['runtime_cleanup_complete'] = True
                self.save()
                return
            time.sleep(.5)
        raise BatchError('batch_product_runtime_cleanup_incomplete', name)

    def rbac(self):
        namespace = self.record['namespace']
        for resource, verb, allowed in [('jobs', 'create', True), ('jobs', 'delete', True), ('pods', 'create', False),
                ('pods', 'delete', False), ('secrets', 'get', False), ('sandboxclaims.extensions.agents.x-k8s.io', 'create', False)]:
            args = [self.lab.tools['kubectl'], '--kubeconfig', str(self.lab.kubeconfig), '--context', 'kind-agentbox-platform-lab',
                    '-n', namespace, 'auth', 'can-i', verb, resource, '--as=system:serviceaccount:' + namespace + ':batch-api']
            completed = subprocess.run(args, capture_output=True, text=True, timeout=15)
            self.require(completed.stdout.strip() == ('yes' if allowed else 'no'), 'batch API RBAC differs')
        self.mark('bounded_api_role_and_no_worker_credentials')

    def cleanup(self, report=None):
        if report is not None:
            self.record = json.loads(Path(report).read_text())
        run = self.record.get('run', '')
        self.require(re.fullmatch('[a-f0-9]{10}', run) and self.record.get('namespace') == 'agentbox-batch-product-' + run,
                     'invalid product fixture report')
        self.state = self.kueue.state
        self.require(self.record['owner'] == self.state['session'], 'foreign product report')
        namespace = self.record['namespace']
        ns_record = next((v for v in self.record['resources'] if v['kind'] == 'Namespace'), None)
        ns = self.get('namespace', namespace)
        if ns is not None:
            self.require(ns_record is not None, 'namespace identity missing')
            identity(ns, ns_record['uid'], self.record['owner'], run)
            cri = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
            self.record['cleanup_sandboxes'] = [s['id'] for s in cri if s.get('metadata', {}).get('namespace') == namespace]
            self.save()
            jobs = json.loads(self.lab.kubectl('-n', namespace, 'get', 'jobs', '-o', 'json', announce=False))['items']
            for job in jobs:
                identity(job, job['metadata']['uid'], self.record['owner'], run)
                name, uid = job['metadata']['name'], job['metadata']['uid']
                self.record['jobs'].setdefault(name, {'name': name, 'uid': uid, 'pods': {}, 'manifest': job})
                self.capture({'batch_id': name + '.' + uid})
                BatchAcceptance(self.lab).delete('Job', name, uid, namespace)
            self.save()
            # Snapshot all compute sandboxes before deleting the namespace.
            cri = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
            sandboxes = [s['id'] for s in cri if s.get('metadata', {}).get('namespace') == namespace]
            self.record['cleanup_sandboxes'] = sorted(set(self.record['cleanup_sandboxes']) | set(sandboxes))
            pvc = self.get('pvc', 'batch-artifacts', namespace)
            if pvc and pvc.get('spec', {}).get('volumeName'):
                pv = self.get('pv', pvc['spec']['volumeName'])
                self.require(pv['spec']['claimRef']['uid'] == pvc['metadata']['uid'] and pv['spec']['persistentVolumeReclaimPolicy'] == 'Delete', 'unowned/non-deleting test volume')
                self.record['pv'] = {'name': pv['metadata']['name'], 'uid': pv['metadata']['uid']}
            self.save()
            BatchAcceptance(self.lab).delete('Namespace', namespace, ns_record['uid'])
            self._wait_absent('namespace', namespace)
        for kind in ('ClusterQueue', 'ResourceFlavor'):
            for obj in [v for v in self.record['resources'] if v['kind'] == kind]:
                actual = self.get(kind, obj['name'])
                if actual:
                    identity(actual, obj['uid'], self.record['owner'], run)
                    BatchAcceptance(self.lab).delete(kind, obj['name'], obj['uid'])
                    self._wait_absent(kind, obj['name'])
        for name in self.record['jobs']:
            self.wait_runtime_cleanup(name)
        for sid in self.record.get('cleanup_sandboxes', []):
            self.require(re.fullmatch('[a-f0-9]{64}', sid) is not None, 'invalid sandbox identity')
            self.require(not self.runtime.processes(sid), 'runtime remains after namespace deletion')
        if self.record.get('pv'):
            self._wait_absent('pv', self.record['pv']['name'])
        self.record['cleanup_complete'] = True
        self.save()
        return {'cleanup_complete': True, 'run': run}

    def _wait_absent(self, kind, name):
        until = time.monotonic() + 90
        while time.monotonic() < until:
            if self.get(kind, name) is None:
                return
            time.sleep(.5)
        raise BatchError('batch_product_cleanup_timeout', kind + '/' + name)

    def test(self):
        with self.lab.operation_lock():
            self.runtime = self.kueue.attach()
            self.state = self.kueue.state
            self.kueue.verify()
            worker = worker_bundle(self.lab)
            self.require(worker['docker_id'] == self.state['docker_id'], 'worker bundle belongs to another Docker daemon')
            sessions = Sessions(self.lab)
            sessions.state = self.state
            run = uuid.uuid4().hex[:10]
            namespace = 'agentbox-batch-product-' + run
            self.record.update(schema_version=1, owner=self.state['session'], run=run, namespace=namespace,
                resources=[], jobs={}, views={}, runtime_evidence={}, worker=worker, all_passed=False)
            self.save()
            try:
                api_image = self.build_image(sessions)
                import_image(self.lab, api_image, self.record['api_image']['id'], self.runtime.worker_name,
                    self.lab.state_dir / 'batch-product-api-image.tar')
                params = {'OWNER': self.state['session'], 'RUN': run, 'NAME': namespace, 'NAMESPACE': namespace,
                    'QUEUE_NAME': namespace, 'FLAVOR_NAME': 'agentbox-batch-product-worker-' + run,
                    'API_IMAGE': api_image, 'WORKER_IMAGE': worker['image'], 'WORKER_IMAGE_ID': worker['image_id'], 'WORKER_IMAGE_DIGEST': worker['image_digest'],
                    'API_INSTANCE': 'batch-' + run}
                values = objects(render(self.lab.config, 'batch-api-resources', params))
                self.budget(values)
                order = {'Namespace': 0, 'ResourceQuota': 1, 'NetworkPolicy': 2, 'ResourceFlavor': 3, 'ClusterQueue': 4,
                    'LocalQueue': 5, 'ServiceAccount': 6, 'Role': 7, 'RoleBinding': 8, 'PersistentVolumeClaim': 9, 'ConfigMap': 10}
                for obj in sorted([o for o in values if o['kind'] != 'Deployment'], key=lambda o: order[o['kind']]):
                    if obj['kind'] == 'NetworkPolicy' and obj['metadata']['name'] == 'batch-api':
                        self.policies(obj)
                    self.create(obj)
                self.alice, self.bob = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
                with tempfile.TemporaryDirectory(prefix='batch-product-private-', dir=self.lab.state_dir) as tmp:
                    self.secret(Path(tmp), namespace, self.alice, self.bob)
                    self.create(next(o for o in values if o['kind'] == 'Deployment'))
                    self.connect(namespace)
                    self.rbac()
                    self.api('POST', '/v2/batches', 'invalid', {'request_id': 'denied', 'dataset_id': worker['dataset']}, expected={401})
                    body = {'request_id': 'image-success', 'dataset_id': worker['dataset'], 'passes': 512}
                    connection = self.connection()
                    connection.request('POST', '/v2/batches', json.dumps(body), {'Authorization': 'Bearer ' + self.alice, 'Content-Type': 'application/json'})
                    response = connection.getresponse()
                    self.require(response.status == 202, 'initial submission failed')
                    response.close()
                    connection.close()  # Deliberately discard the submission body; recover by the same request_id.
                    self.http = None
                    # Closing an unread TLS body may also terminate kubectl's
                    # local tunnel. Reconnect only the transport, never the task.
                    api_uid = self.record['api_pod']['uid']
                    self.stop()
                    self.connect(namespace)
                    self.require(self.record['api_pod']['uid'] == api_uid, 'discarded response unexpectedly restarted the API')
                    self.record['discarded_submit_body_transport_reconnected'] = True
                    _, view = self.api('POST', '/v2/batches', self.alice, body, expected={200})
                    _, replay = self.api('POST', '/v2/batches', self.alice, body, expected={200})
                    self.require(view['batch_id'] == replay['batch_id'], 'duplicate Job identity')
                    self.api('POST', '/v2/batches', self.alice, dict(body, passes=1), expected={409})
                    self.api('GET', '/v2/batches/' + view['batch_id'], self.bob, expected={404})
                    self.wait_running(view)
                    success = self.wait_task(view, 'succeeded')
                    downloaded = self.verify_artifacts(success)
                    self.download(success, success['artifacts'][0]['name'], self.bob, expected=404)
                    self.mark('idempotent_submit_lost_body_conflict_and_principal_isolation')
                    self.mark('real_image_pixels_and_download_checksums')
                    self.api('DELETE', '/v2/batches/' + view['batch_id'], self.alice, expected={200, 202})
                    self.wait_task(view, 'succeeded', cleanup=True)
                    self.wait_runtime_cleanup(view['batch_id'].split('.')[0])
                    old_api = self.record['api_pod']
                    self.stop()
                    path = '/api/v1/namespaces/' + namespace + '/pods/' + old_api['name']
                    options = {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': old_api['uid']}}
                    self.lab.kubectl('delete', '--raw', path, '-f', '-', input_text=json.dumps(options))
                    self.connect(namespace, old_api['uid'])
                    self.require(self.record['api_pod']['uid'] != old_api['uid'], 'API did not restart')
                    self.verify_artifacts(success)
                    _, replay = self.api('POST', '/v2/batches', self.alice, body, expected={200})
                    self.require(replay['batch_id'] == success['batch_id'] and replay['state'] == 'succeeded', 'restart lost durable record')
                    self.mark('artifacts_and_idempotency_survive_job_deletion_and_api_restart')
                    filename, original = next(iter(downloaded.items()))
                    publish_path = '/data/published/' + success['batch_id'].split('.')[0] + '/' + filename
                    self.lab.kubectl('-n', namespace, 'exec', self.record['api_pod']['name'], '-c', 'api', '--',
                        'python3', '-I', '-B', '-c', 'from pathlib import Path;Path(' + repr(publish_path) + ').write_bytes(b"corrupt")')
                    self.download(success, filename, expected=503)
                    self.lab.kubectl('-n', namespace, 'exec', '-i', self.record['api_pod']['name'], '-c', 'api', '--',
                        'python3', '-I', '-B', '-c', 'import sys,base64;from pathlib import Path;Path(' + repr(publish_path) + ').write_bytes(base64.b64decode(sys.stdin.buffer.read()))',
                        input_text=base64.b64encode(original).decode())
                    self.verify_artifacts(success)
                    self.mark('corrupted_published_artifact_is_rejected')
                    _, holder = self.api('POST', '/v2/batches', self.alice, {'request_id': 'running-cancel', 'dataset_id': worker['dataset'], 'passes': 4096}, expected={202})
                    self.wait_running(holder)
                    _, queued = self.api('POST', '/v2/batches', self.alice, {'request_id': 'queued-cancel', 'dataset_id': worker['dataset']}, expected={202})
                    self.wait_task(queued, 'queued')
                    self.capture(queued)
                    self.require(not self.record['jobs'][queued['batch_id'].split('.')[0]]['pods'], 'queued Job created a Pod')
                    self.api('DELETE', '/v2/batches/' + queued['batch_id'], self.alice, expected={200, 202})
                    self.wait_task(queued, 'cancelled', cleanup=True)
                    self.api('DELETE', '/v2/batches/' + holder['batch_id'], self.alice, expected={200, 202})
                    self.wait_task(holder, 'cancelled', cleanup=True)
                    self.wait_runtime_cleanup(holder['batch_id'].split('.')[0])
                    self.download(holder, filename, expected=409)
                    self.mark('queued_and_running_cancel_do_not_publish_partial_output')
                    _, retry = self.api('POST', '/v2/batches', self.alice, {'request_id': 'retry', 'dataset_id': worker['dataset'], 'passes': 512}, expected={202})
                    first = self.wait_running(retry)
                    path = '/api/v1/namespaces/' + namespace + '/pods/' + first['metadata']['name']
                    self.lab.kubectl('delete', '--raw', path, '-f', '-', input_text=json.dumps({'apiVersion': 'v1', 'kind': 'DeleteOptions',
                        'preconditions': {'uid': first['metadata']['uid']}}))
                    second = self.wait_running(retry, first['metadata']['uid'])
                    self.require(second['metadata']['uid'] != first['metadata']['uid'], 'retry reused Pod UID')
                    retried = self.wait_task(retry, 'succeeded')
                    self.verify_artifacts(retried)
                    self.api('DELETE', '/v2/batches/' + retry['batch_id'], self.alice, expected={200, 202})
                    self.wait_task(retry, 'succeeded', cleanup=True)
                    self.wait_runtime_cleanup(retry['batch_id'].split('.')[0])
                    self.mark('retry_attempts_are_isolated_and_publish_one_valid_result')
                    self.stop()
                self.cleanup()
                self.kueue.verify()
                self.record.update(all_passed=True, development_cluster_retained=True)
                return self.record
            finally:
                self.stop()
                self.save()


def cleanup_product(lab, report):
    value = json.loads(Path(report).read_text())
    run = value.get('run', '')
    require(isinstance(run, str) and re.fullmatch('[a-f0-9]{10}', run) is not None and
            value.get('namespace') == 'agentbox-batch-product-' + run and value.get('schema_version') == 1,
            'invalid_batch_cleanup', 'not an image product acceptance record')
    with lab.operation_lock():
        fixture = ProductAcceptance(lab)
        fixture.runtime = fixture.kueue.attach()
        fixture.state = fixture.kueue.state
        fixture.record = value
        return fixture.cleanup()
