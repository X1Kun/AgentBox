"""Fixed image worker builds and the real batch product acceptance fixture."""

from lib.acceptance.clients.batch import BatchClient
import base64
import json
from pathlib import Path
import re
import secrets
import tempfile
import uuid


from lib.core.validation import require
from lib.platform.sandbox import Sessions
from lib.manifests import objects, render


from lib.product.worker_image import import_image, worker_bundle

class ProductAcceptance(BatchClient):


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
