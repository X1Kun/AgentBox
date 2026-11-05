"""Fixed image worker builds and the real batch product acceptance fixture."""

import hashlib
import http.client
import io
import json
from pathlib import Path
import re
import ssl
import subprocess
import time
import urllib.error
import urllib.request
import urllib.parse

from PIL import Image
import yaml

from lib.core.errors import BatchError
from lib.core.kubernetes import delete_resource
from lib.platform.kueue import Kueue
from lib.core.kubernetes import identity, quantity
from lib.manifests import read_yaml
from lib.manifests.network import add_api_egress, discover_api_endpoints


from lib.acceptance.clients.session import SessionClient

class BatchClient(SessionClient):
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
                delete_resource(self.lab, 'Job', name, uid, namespace)
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
            delete_resource(self.lab, 'Namespace', namespace, ns_record['uid'])
            self._wait_absent('namespace', namespace)
        for kind in ('ClusterQueue', 'ResourceFlavor'):
            for obj in [v for v in self.record['resources'] if v['kind'] == kind]:
                actual = self.get(kind, obj['name'])
                if actual:
                    identity(actual, obj['uid'], self.record['owner'], run)
                    delete_resource(self.lab, kind, obj['name'], obj['uid'])
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


