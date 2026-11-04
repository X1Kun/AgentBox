"""Single-worker R/B calibration through real session and image product APIs."""
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import csv
import json
import re
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from lib import baseline_results as results
from lib.boundary_workloads import compile_workload
from lib.observability_acceptance import instrument
from lib.platform_api_execution import APIExecutionAcceptance
from lib.platform_batch import BatchAcceptance, identity, quantity, require
from lib.platform_batch_product import ProductAcceptance, import_image, worker_bundle
from lib.platform_manifests import objects, read_yaml, render
from lib.platform_sessions import Sessions
from lib.session_boundaries import SessionBoundaries
from lib.experiment_diagnostics import ExperimentDiagnostics, archive_metrics


class BaselineProduct(ProductAcceptance):
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
        patch = [{'op': 'test', 'path': '/metadata/resourceVersion', 'value': queue['metadata']['resourceVersion']}]
        for index, value in enumerate((str(max(1, slots)*500)+'m', str(max(1, slots)*256)+'Mi')):
            patch.append({'op': 'replace', 'path': f'/spec/resourceGroups/0/flavors/0/resources/{index}/nominalQuota', 'value': value})
        updated = json.loads(self.lab.kubectl('patch', 'clusterqueue', namespace, '--type=json', '-p', json.dumps(patch), '-o', 'json', announce=False))
        generation = updated['metadata']['generation']
        until = time.monotonic()+45
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
                            BatchAcceptance(self.lab).delete(ref['kind'], ref['name'], ref['uid'])
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


class StaticBaseline(SessionBoundaries):
    def save(self):
        self.lab.write_json(self.output / 'static-baseline.json', self.record)

    def _capture_diagnostics(self, api):
        if not getattr(self, '_recovering', False):
            super()._capture_diagnostics(api)

    def _capture_metrics(self):
        if not getattr(self, '_recovering', False):
            super()._capture_metrics()

    def _archive_observer(self):
        if not getattr(self, '_recovering', False):
            super()._archive_observer()

    def _collect_cleanup_evidence(self, runtime, captured):
        if not getattr(self, '_recovering', False):
            return super()._collect_cleanup_evidence(runtime, captured)
        # A previous pass may already have deleted the namespace. Recover CRI
        # identities by the recorded scope instead of requiring a live Pod list.
        names = {ref['name'] for ref in self.namespaces.values()}
        items = json.loads(runtime.node('crictl', 'pods', '-o', 'json'))['items']
        ids = {s['id'] for s in items if s.get('metadata', {}).get('namespace') in names}
        ids.update(self.record.get('cleanup_sandbox_ids', []))
        self.record['cleanup_sandbox_ids'] = sorted(ids)

    def cleanup(self, report):
        """Resume only recorded identities; keep the original failed run immutable."""
        report = Path(report)
        record = json.loads(report.read_text())
        run = record.get('run', '')
        require(re.fullmatch('[a-f0-9]{10}', run) is not None, 'invalid_baseline_report', 'invalid run')
        for role, ref in record.get('namespaces', {}).items():
            require(role in ('work', 'client') and ref.get('name') == 'agentbox-baseline-'+role+'-'+run and ref.get('uid'),
                    'invalid_baseline_report', 'namespace identity missing')
        product_path = report.parent/'batch-product-test.json'
        product_record = json.loads(product_path.read_text()) if product_path.exists() else None
        if product_record:
            namespace = 'agentbox-batch-product-'+run
            require(product_record.get('run') == run and product_record.get('namespace') == namespace and
                    product_record.get('owner') == record.get('owner'), 'invalid_baseline_report', 'batch identity differs')
            names = {'Namespace': namespace, 'ClusterQueue': namespace,
                     'ResourceFlavor': 'agentbox-batch-product-worker-'+run}
            require(all(r['name'] == names[r['kind']] for r in product_record.get('resources', []) if r['kind'] in names),
                    'invalid_baseline_report', 'cluster resource outside this fixture')
        with self.lab.operation_lock():
            sessions = Sessions(self.lab)
            runtime = sessions.attach()
            require(record.get('owner') == sessions.state['session'], 'foreign_baseline_report', 'cluster owner differs')
            self.record = record
            self._recovering = True
            self.record.update(cleanup_recovery_of=str(report), all_passed=False, teardown_errors=[])
            self.namespaces = dict(record.get('namespaces', {}))
            api = APIExecutionAcceptance(self.lab)
            product = BaselineProduct(self.lab)
            product.runtime, product.state = runtime, sessions.state
            try:
                if product_record:
                    product.record = product_record
                    product.teardown()
            finally:
                try:
                    self._teardown(api, sessions, runtime, self.record.get('runtime_evidence', {}), run)
                finally:
                    self.record['batch_cleanup_complete'] = product.record.get('cleanup_complete', not product_record)
                    self.record['cleanup_complete'] = self.record.get('cleanup_complete', False) and self.record['batch_cleanup_complete']
                    self.save()
            return self.record

    def budget(self, runtime, config):
        node = json.loads(self.lab.kubectl('get', 'node', runtime.worker_name, '-o', 'json', announce=False))
        pods = json.loads(self.lab.kubectl('get', 'pods', '-A', '--field-selector=spec.nodeName='+runtime.worker_name, '-o', 'json', announce=False))['items']
        required = {'cpu': str(config['session_pod_budget']*500+2000)+'m',
                    'memory': str(config['session_pod_budget']*512+1024)+'Mi'}
        existing = {key: quantity('0') for key in required}
        for pod in pods:
            if pod.get('status', {}).get('phase') in ('Succeeded', 'Failed'):
                continue
            for key in existing:
                regular = sum((quantity(c.get('resources', {}).get('requests', {}).get(key, '0')) for c in pod['spec']['containers']), quantity('0'))
                init = max((quantity(c.get('resources', {}).get('requests', {}).get(key, '0')) for c in pod['spec'].get('initContainers', [])), default=quantity('0'))
                existing[key] += max(regular, init) + quantity(pod['spec'].get('overhead', {}).get(key, '0'))
        for key in required:
            require(existing[key]+quantity(required[key]) <= quantity(node['status']['allocatable'][key]),
                    'baseline_insufficient_capacity', key)
        self.record['budget'] = {'required_limits': required, 'existing_requests': {k: str(v) for k, v in existing.items()},
                                 'allocatable': node['status']['allocatable'],
                                 'scope': 'single kind worker; nodes share physical host; no physical reservation'}
        self.save()

    def export_metrics(self, directory, start, end):
        return archive_metrics(self.stack, directory/'prometheus-range.json', start, end)

    def snapshot(self, product, directory, elapsed):
        namespace = product.record['namespace']
        snapshot = {'elapsed_seconds': elapsed, 'at': time.time()}
        for resource in ('jobs', 'pods', 'workloads.kueue.x-k8s.io'):
            snapshot[resource] = json.loads(self.lab.kubectl('-n', namespace, 'get', resource, '-o', 'json', announce=False))['items']
        cri = json.loads(product.runtime.node('crictl', 'pods', '-o', 'json'))['items']
        snapshot['cri_sandboxes'] = [s for s in cri if s.get('metadata', {}).get('namespace') == namespace]
        product.record['captured_sandbox_ids'] = sorted(set(product.record.get('captured_sandbox_ids', [])) |
            {s['id'] for s in snapshot['cri_sandboxes']})
        product.save()
        with (directory / 'batch-resources.jsonl').open('a') as stream:
            stream.write(json.dumps(snapshot, separators=(',', ':'))+'\n')

    def cell(self, cell, config, targets, api, token, product, users_path, private, binary):
        directory = self.output / cell['id']
        directory.mkdir()
        self.record['current_cell'] = cell['id']
        self.save()
        print('[platform] static baseline: ' + cell['id'], flush=True)
        product.quota(cell['batch_slots'])
        self.lab.write_json(directory/'queue.json', product.record['effective_queue'])
        self.wait('agentbox_inventory_items{domain="session",kind="pool",state="ready"} == 2')
        trace_requests = []
        for request in cell['requests']:
            code, expected, exit_code = compile_workload(config['work'], self.record['run']+':'+str(request['target']))
            trace_requests.append(dict(request, action='execute', code=code, expected_out=expected,
                                       expected_exit=exit_code, timeout_seconds=20))
        start = time.time()+5
        anchor = time.monotonic()+5
        trace = {'start_at': datetime.fromtimestamp(start, timezone.utc).isoformat(),
                 'targets': [{'id': t['session_id'], 'revision': t['identity_revision'], 'user': 'alice'} for t in targets],
                 'requests': trace_requests, 'max_inflight': config['max_inflight'],
                 'max_lag_ms': config['max_lag_ms'], 'deadline_ms': config['deadline_ms']}
        self.lab.write_json(private / 'trace.json', trace)
        self.lab.write_json(directory / 'trace.json', trace)
        entries = [{'request_id': cell['id'].lower()+'-'+str(i), 'planned_ms': at}
                   for i, at in enumerate(config['batch']['arrivals_ms'])] if cell['batch_slots'] else []
        ledger_path = directory / 'batch-ledger.json'

        def submit(entry):
            entry = dict(entry)
            delay = anchor+entry['planned_ms']/1000-time.monotonic()
            if delay > 0:
                time.sleep(delay)
            entry['sent_ms'] = (time.monotonic()-anchor)*1000
            if entry['sent_ms']-entry['planned_ms'] > config['max_lag_ms']:
                entry['outcome'] = 'dropped'
                return entry
            body = {'request_id': entry['request_id'], 'dataset_id': product.record['worker']['dataset'],
                    'passes': config['batch']['passes']}
            request = urllib.request.Request(product.base+'/v2/batches', method='POST',
                data=json.dumps(body).encode(), headers={'Authorization': 'Bearer '+product.alice, 'Content-Type': 'application/json'})
            try:
                try:
                    response = urllib.request.urlopen(request, context=product.tls_context, timeout=15)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    value = json.loads(response.read(1 << 20))
                    entry.update(http_status=response.status, response=value,
                                 outcome='accepted' if response.status in (200, 202) else
                                     ('unknown' if response.status >= 500 else 'rejected'))
                    if value.get('batch_id'):
                        entry['batch_id'] = value['batch_id']
            except Exception as error:
                entry.update(outcome='unknown', error_type=type(error).__name__)
            entry['submit_finished_seconds'] = time.monotonic()-anchor
            return entry

        process = subprocess.Popen([binary, '--config', private/'trace.json', '--ledger', directory/'session.jsonl',
            '--api-url', api.base, '--api-ca-file', private/'tls.crt', '--users-file', users_path],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            with ThreadPoolExecutor(max_workers=4) as executor:
                futures = {executor.submit(submit, entry): entry for entry in entries}
                received = set()
                verified = set()
                until = anchor+config['window_ms']/1000+config['drain_seconds']
                while time.monotonic() < until:
                    elapsed = time.monotonic()-anchor
                    if elapsed < 0:
                        time.sleep(min(.5, -elapsed))
                        continue
                    for future in futures:
                        if future.done() and future not in received:
                            futures[future].update(future.result())
                            received.add(future)
                    for entry in entries:
                        if not entry.get('batch_id') or entry['request_id'] in verified:
                            continue
                        _, view = product.api('GET', '/v2/batches/'+entry['batch_id'], product.alice, expected={200})
                        entry['last_view'] = view
                        entry.setdefault('observations', []).append({'at_seconds': time.monotonic()-anchor, 'state': view['state']})
                        if view['state'] == 'succeeded':
                            product.verify_artifacts(view)
                            entry['verified_at'] = time.monotonic()-anchor
                            verified.add(entry['request_id'])
                    self.lab.write_json(ledger_path, entries)
                    self.snapshot(product, directory, time.monotonic()-anchor)
                    if time.monotonic() >= anchor+config['window_ms']/1000 and process.poll() is not None and all(f.done() for f in futures) and all(e.get('verified_at') is not None or e.get('outcome') != 'accepted' for e in entries):
                        break
                    time.sleep(2)
            stdout, stderr = process.communicate(timeout=80)
            require(process.returncode == 0, 'baseline_driver_failed', stderr[-300:])
            self.lab.write_json(directory/'driver-summary.json', json.loads(stdout))
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.communicate(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.communicate(timeout=5)
            self.lab.write_json(ledger_path, entries)
        events = [json.loads(line) for line in (directory/'session.jsonl').read_text().splitlines()]
        snapshots = [json.loads(line) for line in (directory/'batch-resources.jsonl').read_text().splitlines()]
        results.batch_stages(snapshots, entries)
        self.lab.write_json(ledger_path, entries)
        session = results.session_summary(trace, events, config)
        require(not session['outcomes'].get('wrong'), 'baseline_wrong_result', cell['id'])
        metrics = self.export_metrics(directory, start, time.time())
        observation = results.observations(metrics, start, start+config['window_ms']/1000)
        self.snapshot(product, directory, time.monotonic()-anchor)
        summary = dict(cell, session=session, batch=results.batch_summary(entries, config['window_ms']/1000, config['batch']['passes']),
                       batch_generator_valid=all(e.get('outcome') != 'dropped' for e in entries),
                       trace_sha256=results.digest(trace), started_at=start,
                       measurement_valid=session['measurement_valid'] and observation['inventory_and_scrapes_valid']
                           and all(e.get('outcome') != 'dropped' for e in entries),
                       observation=observation,
                       metric_series={key: len(value['data']['result']) for key, value in metrics.items()})
        summary.pop('requests')
        self.lab.write_json(directory/'summary.json', summary)
        self.record.setdefault('cells', []).append(summary)
        self.record['aggregate'] = results.aggregate(self.record['cells'])
        with (self.output/'rounds.csv').open('w', newline='') as stream:
            writer = csv.writer(stream)
            writer.writerow(['cell', 'group', 'profile', 'repetition', 'measurement_valid', 'timely_ratio',
                             'p50_ms', 'p95_ms', 'p99_ms', 'batch_verified_passes_per_second', 'backlog_at_window'])
            for item in self.record['cells']:
                writer.writerow([item['id'], item['group'], item['profile'], item['repetition'], item['measurement_valid'],
                    item['session']['timely_ratio'], *[item['session']['all_sent_latency_ms']['p'+str(p)] for p in (50, 95, 99)],
                    item['batch']['verified_passes_per_second'], item['batch']['backlog_at_window']])
        self.save()
        require(not any(e.get('outcome') == 'unknown' and not e.get('batch_id') for e in entries),
                'baseline_unresolved_submission', 'unknown durable intent must not carry into the next cell')
        # Drain completions remain outside window throughput. Cancel anything still
        # unfinished, then prove no Job/Workload/Pod can carry into the next cell.
        for entry in entries:
            if entry.get('batch_id'):
                product.api('DELETE', '/v2/batches/'+entry['batch_id'], product.alice, expected={200, 202})
                product.wait_task({'batch_id': entry['batch_id']}, entry.get('last_view', {}).get('state')
                    if entry.get('last_view', {}).get('state') in ('succeeded', 'failed') else 'cancelled', seconds=90, cleanup=True)
        jobs = json.loads(self.lab.kubectl('-n', product.record['namespace'], 'get', 'jobs', '-o', 'json', announce=False))['items']
        require(not jobs, 'baseline_unresolved_submission', 'no next cell while an unknown Job remains')
        for resource in ('workloads.kueue.x-k8s.io', 'pods'):
            values = json.loads(self.lab.kubectl('-n', product.record['namespace'], 'get', resource, '-o', 'json', announce=False))['items']
            require(not [v for v in values if resource != 'pods' or v['metadata'].get('labels', {}).get('agentbox.dev/batch-role') == 'compute'],
                    'baseline_cell_not_drained', resource)
        pod_uids = {p['metadata']['uid'] for snapshot in snapshots for p in snapshot['pods']
                    if p['metadata'].get('labels', {}).get('agentbox.dev/batch-role') == 'compute'}
        sandbox_ids = {s['id'] for snapshot in snapshots for s in snapshot.get('cri_sandboxes', [])
                       if s.get('labels', {}).get('io.kubernetes.pod.uid') in pod_uids}
        until = time.monotonic()+90
        while True:
            live = {sid: product.runtime.processes(sid) for sid in sandbox_ids}
            if not any(live.values()):
                break
            require(time.monotonic() < until, 'baseline_cell_runtime_remaining', cell['id'])
            time.sleep(.5)
        self.lab.write_json(directory/'cleanup.json', {'jobs_workloads_compute_pods_gone': True,
            'captured_compute_sandbox_ids': sorted(sandbox_ids), 'remaining_processes': live})

    def test(self, config_path, stage='calibration', reference_path=None):
        config = results.validate(read_yaml(config_path))
        reference = json.loads(Path(reference_path).read_text()) if reference_path else None
        cells = results.plan(config, stage, reference)
        with self.lab.operation_lock():
            sessions = Sessions(self.lab)
            runtime = sessions.attach()
            bundle = sessions.bundle(require_client=False)
            sessions.verify(bundle)
            if reference is not None:
                require(reference.get('owner') == sessions.state['session'] and
                        reference.get('platform_identity', {}).get('node_ids') == sessions.state['node_ids'] and
                        reference.get('platform_identity', {}).get('gvisor') == sessions.state.get('gvisor'),
                        'baseline_reference_platform_changed', 'comparison requires the original platform')
            product = BaselineProduct(self.lab)
            product.kueue.attach()
            product.kueue.verify()
            worker = worker_bundle(self.lab)
            require(worker['docker_id'] == sessions.state['docker_id'], 'foreign_baseline_worker', 'worker belongs to another Docker daemon')
            run = uuid.uuid4().hex[:10]
            self.record.update(run=run, owner=sessions.state['session'], started_at=time.time(), config=config,
                               config_sha256=results.digest(config), plan=cells,
                               scope=config['scope'], complete=False, stage=stage)
            if reference is not None:
                self.record.update(reference_report=str(reference_path), reference_sha256=results.digest(reference),
                                   eligible_profiles=results.eligible_profiles(config, reference))
            self.record['platform_identity'] = {'docker_id': sessions.state['docker_id'],
                'node_ids': sessions.state['node_ids'], 'gvisor': sessions.state.get('gvisor'),
                'kueue': sessions.state.get('kueue')}
            self.save()
            self.budget(runtime, config)
            api = APIExecutionAcceptance(self.lab)
            api.record.update(runtime_evidence={}, owner=sessions.state['session'], run=run)
            captured = {}
            sampler = None
            try:
                self.stack.up()
                sampler = ExperimentDiagnostics(self.lab, self.output)
                sampler.start()
                image = api.build_image(sessions)
                if reference is not None:
                    require(reference.get('api_image', {}).get('id') == api.record['api_image']['id'] and
                            reference.get('runtime_image') == bundle['images']['runtime'] and
                            all(reference.get('worker', {}).get(k) == worker[k] for k in ('image_id', 'image_digest', 'dataset_sha256')),
                            'baseline_reference_images_changed', 'freeze product/runtime/worker images before comparison')
                self.record['api_image'] = api.record['api_image']
                self.record['runtime_image'] = bundle['images']['runtime']
                self.record['worker'] = worker
                self.save()
                import_image(self.lab, image, api.record['api_image']['id'], runtime.worker_name,
                             self.lab.state_dir/'baseline-api-image.tar')
                for role in ('work', 'client'):
                    self.namespaces[role] = sessions.namespace('agentbox-baseline-'+role+'-'+run, 'baseline-'+role, run)
                self.record['namespaces'] = self.namespaces
                self.save()
                work, client = self.namespaces['work']['name'], self.namespaces['client']['name']
                params = {'OWNER': sessions.state['session'], 'RUN': run, 'WORK_NS': work, 'CLIENT_NS': client,
                          'RUNTIME_IMAGE': bundle['images']['runtime']['name'], 'API_IMAGE': image}
                for name in ('session-warm', 'api-rbac', 'api-quota'):
                    obj = sessions.render(name, params)
                    if name == 'api-quota':
                        n = config['session_pod_budget']
                        obj['spec']['hard'].update(pods=str(n), **{'requests.cpu': str(n*100)+'m',
                            'requests.memory': str(n*128)+'Mi', 'limits.cpu': str(n*500)+'m', 'limits.memory': str(n*512)+'Mi'})
                    self.record.setdefault('effective_session_resources', []).append(obj)
                    sessions.create(obj)
                sessions.create(sessions.client_policy(client, work, run))
                binary = self.lab.state_dir/'agentbox-session-experiment'
                self.lab.runner.run(['go', 'build', '-trimpath', '-buildvcs=false', '-o', binary, './cmd/agentbox-session-experiment'], timeout=900)
                with tempfile.TemporaryDirectory(prefix='baseline-private-', dir=self.lab.state_dir) as tmp:
                    private = Path(tmp)
                    batch_private = private/'batch'
                    batch_private.mkdir()
                    product.start(sessions, runtime, image, worker, batch_private, run)
                    token = secrets.token_urlsafe(32)
                    api.secret(private, client, token, secrets.token_urlsafe(32))
                    users = private/'users.json'
                    self.lab.write_json(users, {'alice': token})
                    users.chmod(0o600)
                    deployment = instrument(self.lab, sessions.render('api', params))
                    args = deployment['spec']['template']['spec']['containers'][0]['args']
                    args[args.index('--pool=python-cold')] = '--pool=python-warm'
                    args.append('--default-ttl-seconds=3600')
                    sessions.create(deployment)
                    api.connect(client)
                    self.attach('session', work, api.record['api_pod'], client)
                    self.attach('batch', product.record['namespace'], product.record['api_pod'])
                    sampler.set_scope([*self.namespaces.values(), next(r for r in product.record['resources'] if r['kind'] == 'Namespace')])
                    current_cohort, targets = None, []
                    for cell in cells:
                        if cell['cohort'] != current_cohort:
                            for view in targets:
                                api.close(view, token)
                            current_cohort = cell['cohort']
                            targets = []
                            for i in range(config['session_count']):
                                before = time.time()
                                view = api.session(f'{current_cohort}-session-{i}', token, runtime, work)
                                targets.append(view)
                                api.file(view, token, 'marker', (run+':'+str(i)).encode())
                                _, warm = api.execute(view, token, 'print(42)', expected={200})
                                require(warm['stdout'] == '42\n', 'baseline_warmup_wrong', str(i))
                                self.record.setdefault('session_setup', []).append({'cohort': current_cohort,
                                    'index': i, 'started_at': before, 'ready_at': time.time(), 'view': view})
                            captured.update(api.record['runtime_evidence'])
                        self.cell(cell, config, targets, api, token, product, users, private, binary)
                        summary = self.record['cells'][-1]
                        require(summary['session']['measurement_valid'] and summary['batch_generator_valid'], 'baseline_invalid_generator', cell['id'])
                        require(summary['observation']['inventory_and_scrapes_valid'], 'baseline_observation_invalid', cell['id'])
                    for view in targets:
                        api.close(view, token)
                    self.export_metrics(self.output, self.record['started_at'], time.time())
                    self.record.update(complete=True, all_passed=True,
                        repeated_baseline_complete=config['repetitions'] >= 5,
                        aggregate=results.aggregate(self.record['cells']))
                    self.save()
                    return self.record
            finally:
                primary = sys.exc_info()[1]
                if primary is not None:
                    self.record['primary_failure'] = {'type': type(primary).__name__, 'reason': getattr(primary, 'reason', None)}
                # Independent teardown paths: batch errors cannot skip session GC.
                if sampler:
                    sampler.begin_cleanup()
                try:
                    if product.record.get('namespace'):
                        product.teardown()
                finally:
                    try:
                        self._teardown(api, sessions, runtime, captured, run)
                    finally:
                        if sampler:
                            self.record['diagnostics_sampler'] = sampler.stop()
                        self.record['batch_cleanup_complete'] = product.record.get('cleanup_complete', not product.record.get('namespace'))
                        self.record['cleanup_complete'] = self.record.get('cleanup_complete', False) and self.record['batch_cleanup_complete']
                        if not self.record['cleanup_complete']:
                            self.record['all_passed'] = False
                        self.save()
