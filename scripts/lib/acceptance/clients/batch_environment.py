"""Single-worker R/B calibration through real session and image product APIs."""

import json
import secrets
import time
from lib.core.errors import CommandError

from lib.observability.environment import instrument
from lib.core.kubernetes import delete_resource
from lib.core.kubernetes import identity
from lib.core.validation import require
from lib.acceptance.clients.batch import BatchClient
from lib.manifests import objects, render


class BatchEnvironment(BatchClient):
    def start(self, sessions, runtime, image, worker, directory, run):
        self.runtime, self.state = runtime, sessions.state
        namespace = 'agentbox-batch-product-' + run
        self.record.update(schema_version=1, owner=self.state['session'], run=run,
                           namespace=namespace, resources=[], jobs={}, views={},
                           runtime_evidence={}, worker=worker, cleanup_complete=False)
        self.save()
        params = {'OWNER': self.state['session'], 'RUN': run, 'NAME': namespace,
                  'NAMESPACE': namespace, 'QUEUE_NAME': namespace,
                  'FLAVOR_NAME': 'agentbox-batch-product-worker-' + run,
                  'API_IMAGE': image, 'WORKER_IMAGE': worker['image'],
                  'WORKER_IMAGE_ID': worker['image_id'], 'WORKER_IMAGE_DIGEST': worker['image_digest'],
                  'API_INSTANCE': 'batch-' + run}
        values = objects(render(self.lab.config, 'batch-api-resources', params))
        order = {'Namespace': 0, 'ResourceQuota': 1, 'NetworkPolicy': 2, 'ResourceFlavor': 3,
                 'ClusterQueue': 4, 'LocalQueue': 5, 'ServiceAccount': 6, 'Role': 7,
                 'RoleBinding': 8, 'PersistentVolumeClaim': 9, 'ConfigMap': 10}
        for obj in sorted((o for o in values if o['kind'] != 'Deployment'), key=lambda o: order[o['kind']]):
            if obj['kind'] == 'ResourceQuota':
                # The same total namespace budget applies to every R/B cell.
                obj['spec']['hard'].update(pods='3', **{'requests.cpu': '1100m',
                    'requests.memory': '640Mi', 'limits.cpu': '2000m', 'limits.memory': '1024Mi'})
            if obj['kind'] == 'NetworkPolicy' and obj['metadata']['name'] == 'batch-api':
                self.policies(obj)
            self.create(obj)
        self.alice, self.bob = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
        self.secret(directory, namespace, self.alice, self.bob)
        self.create(instrument(self.lab, next(o for o in values if o['kind'] == 'Deployment')))
        self.connect(namespace)
        self.record['effective_resources'] = values
        self.save()

    def quota(self, slots):
        namespace = self.record['namespace']
        queue = self.get('clusterqueue', namespace)
        identity(queue, next(r['uid'] for r in self.record['resources'] if r['kind'] == 'ClusterQueue'),
                 self.state['session'], self.record['run'])
        until = time.monotonic()+45
        desired=(str(max(1,slots)*500)+'m',str(max(1,slots)*256)+'Mi')
        for attempt in range(5):
            groups=queue.get('spec',{}).get('resourceGroups',[])
            resources=groups[0]['flavors'][0]['resources'] if groups else []
            if len(resources)==2 and tuple(r['nominalQuota'] for r in resources)==desired:
                updated=queue
                break
            patch = [{'op':'test','path':'/metadata/uid','value':queue['metadata']['uid']},
                     {'op':'test','path':'/metadata/resourceVersion','value':queue['metadata']['resourceVersion']}]
            for index,value in enumerate(desired):
                patch.append({'op':'replace','path':f'/spec/resourceGroups/0/flavors/0/resources/{index}/nominalQuota','value':value})
            try:
                updated=json.loads(self.lab.kubectl('patch','clusterqueue',namespace,'--type=json','-p',json.dumps(patch),'-o','json',announce=False))
                break
            except CommandError as error:
                # Only a definite server rejection can be retried. A timeout or
                # lost response remains unknown; never blindly replay a write.
                definite=error.reason=='command_failed' and ('(Conflict)' in str(error) or 'request is invalid' in str(error))
                if not definite or attempt==4 or time.monotonic()>=until:raise
                fresh=self.get('clusterqueue',namespace)
                identity(fresh,queue['metadata']['uid'],self.state['session'],self.record['run'])
                require(fresh.get('spec')==queue.get('spec') and fresh['metadata']['resourceVersion']!=queue['metadata']['resourceVersion'],
                        'baseline_quota_changed','only unrelated status/metadata CAS conflicts may be retried')
                self.record['quota_cas_conflicts']=self.record.get('quota_cas_conflicts',0)+1
                queue=fresh
        generation = updated['metadata']['generation']
        while True:
            current = self.get('clusterqueue', namespace)
            identity(current, queue['metadata']['uid'], self.state['session'], self.record['run'])
            require(current['metadata']['generation'] == generation, 'baseline_quota_changed', namespace)
            if any(c['type'] == 'Active' and c['status'] == 'True' and c.get('observedGeneration', 0) >= generation
                   for c in current.get('status', {}).get('conditions', [])):
                updated = current
                break
            require(time.monotonic() < until, 'baseline_quota_not_converged', namespace)
            time.sleep(.5)
        self.record['effective_queue'] = updated
        self.save()

    def teardown(self):
        """Scoped deletion must survive missing CRI/log evidence and partial setup."""
        errors, sandboxes = [], list(self.record.get('captured_sandbox_ids', []))

        def attempt(stage, operation):
            before, tick = time.time(), time.monotonic()
            outcome = 'complete'
            try:
                operation()
            except Exception as error:
                outcome = 'error'
                errors.append({'stage': stage, 'type': type(error).__name__, 'reason': getattr(error, 'reason', None)})
            finally:
                self.record.setdefault('teardown_timeline', []).append({'stage': stage, 'started_at': before,
                    'ended_at': time.time(), 'duration_seconds': time.monotonic()-tick, 'outcome': outcome})

        namespace = self.record.get('namespace')
        attempt('batch_forward_stop', self.stop)
        try:
            if namespace:
                def capture():
                    items = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
                    sandboxes.extend(s['id'] for s in items if s.get('metadata', {}).get('namespace') == namespace)
                    pvc = self.get('pvc', 'batch-artifacts', namespace)
                    if pvc and pvc.get('spec', {}).get('volumeName'):
                        pv = self.get('pv', pvc['spec']['volumeName'])
                        require(pv['spec']['claimRef']['uid'] == pvc['metadata']['uid'] and
                                pv['spec']['persistentVolumeReclaimPolicy'] == 'Delete', 'unowned_baseline_volume', namespace)
                        self.record['pv'] = pv['metadata']['name']
                attempt('batch_runtime_capture', capture)
        finally:
            for kind in ('Namespace', 'ClusterQueue', 'ResourceFlavor'):
                for ref in (r for r in self.record.get('resources', []) if r['kind'] == kind):
                    def delete(ref=ref):
                        actual = self.get(ref['kind'], ref['name'])
                        if actual:
                            identity(actual, ref['uid'], self.state['session'], self.record['run'])
                            delete_resource(self.lab, ref['kind'], ref['name'], ref['uid'])
                            self._wait_absent(ref['kind'], ref['name'])
                    attempt('batch_delete_' + kind, delete)
            def confirm_runtime():
                until = time.monotonic()+90
                while True:
                    items = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
                    remaining = [s['id'] for s in items if s.get('metadata', {}).get('namespace') == namespace]
                    live = {sid: self.runtime.processes(sid) for sid in sandboxes}
                    self.record['remaining_sandboxes'] = remaining
                    self.record['remaining_processes'] = live
                    if not remaining and not any(live.values()):
                        return
                    require(time.monotonic() < until, 'baseline_runtime_remaining', namespace)
                    time.sleep(.5)
            attempt('batch_runtime_confirmation', confirm_runtime)
            if self.record.get('pv'):
                attempt('batch_volume_confirmation', lambda: self._wait_absent('pv', self.record['pv']))
            self.record.update(teardown_errors=errors, cleanup_complete=not errors,
                               cleanup_sandbox_ids=sandboxes)
            self.save()
        require(not errors, 'baseline_batch_teardown_incomplete', 'see batch-product-test.json')

