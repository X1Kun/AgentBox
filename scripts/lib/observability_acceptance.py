"""Real metrics, missing-data and recovery checks; never runs an R/B benchmark."""
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request
import uuid

import yaml

from lib.platform_api_execution import APIExecutionAcceptance
from lib.platform_batch import BatchError, require, identity
from lib.platform_batch_product import ProductAcceptance
from lib.platform_manifests import read_yaml
from lib.platform_observability import Observability
from lib.platform_sessions import Sessions


def instrument(lab, obj):
    patches = read_yaml(lab.config / 'agentbox/overlays/lab/api-metrics-patch.yaml', mapping=False)
    container = obj['spec']['template']['spec']['containers'][0]
    container['args'].append(patches[0]['value'])
    container['ports'].append(patches[1]['value'])
    return obj


class Forward:
    def __init__(self, lab, namespace, pod, path):
        self.path = path
        self.log = path.open('w')
        self.process = subprocess.Popen([lab.tools['kubectl'], '--kubeconfig', str(lab.kubeconfig),
            '--context', 'kind-agentbox-platform-lab', '-n', namespace, 'port-forward', '--address=127.0.0.1',
            'pod/' + pod, ':9091'], stdin=subprocess.DEVNULL, stdout=self.log, stderr=self.log)
        self.address = None
        until = time.monotonic() + 20
        while time.monotonic() < until:
            require(self.process.poll() is None, 'metrics_forward_failed', 'metrics forward exited')
            for line in path.read_text().splitlines():
                if line.startswith('Forwarding from 127.0.0.1:'):
                    self.address = line.split(' ')[2]
                    return
            time.sleep(.1)
        self.close()
        raise BatchError('metrics_forward_timeout', 'metrics forward was not ready')

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill(); self.process.wait(timeout=5)
        self.log.close()


class ObservedProduct(ProductAcceptance):
    def __init__(self, harness):
        super().__init__(harness.lab)
        self.harness = harness

    def create(self, obj):
        if obj['kind'] == 'Deployment':
            instrument(self.lab, obj)
        return super().create(obj)

    def connect(self, namespace, previous_uid=None):
        super().connect(namespace, previous_uid)
        self.harness.attach('batch', namespace, self.record['api_pod'])

    def mark(self, name):
        super().mark(name)
        if name == 'real_image_pixels_and_download_checksums':
            self.harness.check_batch_observation(self)
        elif name == 'retry_attempts_are_isolated_and_publish_one_valid_result':
            for expr in ('agentbox_state_transitions_total{mode="batch",to="cancelled"} >= 1',
                         'agentbox_operations_total{mode="batch",operation="artifact_read",outcome="error"} >= 1',
                         'agentbox_operations_total{mode="batch",operation="publication",outcome="complete"} >= 1'):
                self.harness.wait(expr)
            self.harness.mark('post_restart_cancel_publication_and_artifact_failure_metrics')


class ObservationAcceptance:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.stack = Observability(lab)
        self.namespaces = {}
        self.forwards = {}
        self.scope = {}
        self.api_uids = {}
        self.record = {'schema_version': 1, 'checks': [], 'cleanup_complete': False, 'all_passed': False}

    def save(self):
        self.lab.write_json(self.output / 'observability-test.json', self.record)

    def mark(self, name):
        self.record['checks'].append({'name': name, 'passed': True}); self.save()

    def query(self, expr):
        data = json.loads(self.stack.http(19090, '/api/v1/query?' + urllib.parse.urlencode({'query': expr})))
        require(data['status'] == 'success', 'prometheus_query_failed', 'query failed')
        return data['data']['result']

    def wait(self, expr, predicate=lambda values: bool(values), timeout=45):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            result = self.query(expr)
            if predicate(result):
                self.record.setdefault('queries', {})[expr] = result; self.save(); return result
            time.sleep(.5)
        raise BatchError('observation_not_converged', expr)

    def attach(self, domain, namespace, pod, api_namespace=None):
        if self.api_uids.get(domain) != pod['uid']:
            if domain in self.forwards:
                self.forwards.pop(domain).close()
            self.forwards[domain] = Forward(self.lab, api_namespace or namespace, pod['name'], self.output / (domain + '-metrics-forward.log'))
            self.api_uids[domain] = pod['uid']
            self.stack.targets({key: value.address for key, value in self.forwards.items()})
        if self.scope.get(domain) != namespace:
            self.scope[domain] = namespace
            self.stack.watch(self.scope)
        self.wait('up{job="agentbox-api",instance="' + domain + '"} == 1')
        self.wait('agentbox_inventory_valid{domain="' + domain + '"} == 1')

    def check_batch_observation(self, product):
        self.wait('agentbox_operations_total{mode="batch",operation="publication",outcome="complete"} >= 1')
        self.wait('agentbox_lifecycle_events_total{domain="batch",kind="job",stage="admitted"} >= 1')
        self.mark('batch_publication_and_kubernetes_stage_observations')
        # Only stop the metrics channel; business TLS traffic must still work.
        self.forwards.pop('batch').close()
        self.wait('up{job="agentbox-api",instance="batch"} == 0')
        product.api('GET', '/healthz', '', expected={200})
        missing = 'agentbox_observer_scope{domain="batch"} unless on(domain) label_replace(up{job="agentbox-api"} == 1,"domain","$1","instance","(session|batch)")'
        self.wait(missing)
        self.mark('metrics_target_loss_is_unknown_while_business_remains_alive')
        self.api_uids.pop('batch', None)
        self.attach('batch', product.record['namespace'], product.record['api_pod'])
        namespace = product.record['namespace']
        role = product.get('role', 'agentbox-observer', namespace)
        identity(role, role['metadata']['uid'], product.record['owner'])
        original = role['rules']
        def patch(rules):
            body = [{'op': 'test', 'path': '/metadata/uid', 'value': role['metadata']['uid']},
                    {'op': 'replace', 'path': '/rules', 'value': rules}]
            self.lab.kubectl('-n', namespace, 'patch', 'role', 'agentbox-observer', '--type=json', '-p', json.dumps(body), announce=False)
        patch([])
        try:
            self.wait('agentbox_inventory_valid{domain="batch"} == 0')
            self.wait('absent(agentbox_inventory_items{domain="batch"})')
            self.wait('up{job="observer"} == 1')
            self.mark('read_permission_loss_omits_inventory_instead_of_zero_fill')
        finally:
            patch(original)
        self.wait('agentbox_inventory_valid{domain="batch"} == 1')
        self.stack.watch(self.scope)
        self.wait('agentbox_inventory_valid{domain="batch"} == 1')
        self.wait('agentbox_lifecycle_events_total{domain="batch",kind="job",stage="submitted",origin="initial"} >= 1')
        self.mark('observer_restart_recovers_initial_events_separately')

    def test(self):
        with self.lab.operation_lock():
            sessions = Sessions(self.lab); runtime = sessions.attach(); bundle = sessions.bundle(); sessions.verify(bundle)
            run = uuid.uuid4().hex[:10]
            self.record.update(run=run, owner=sessions.state['session'], started_at=time.time())
            self.save()
            self.stack.up()
            api = APIExecutionAcceptance(self.lab)
            product = None
            api.record.update(runtime_evidence={}, owner=sessions.state['session'], run=run)
            try:
                image = api.build_image(sessions)
                for role in ('work', 'client'):
                    self.namespaces[role] = sessions.namespace('agentbox-obs-' + role + '-' + run, 'obs-' + role, run)
                self.record['session_namespaces'] = self.namespaces; self.save()
                work, client = self.namespaces['work']['name'], self.namespaces['client']['name']
                params = {'OWNER': sessions.state['session'], 'RUN': run, 'WORK_NS': work, 'CLIENT_NS': client,
                          'RUNTIME_IMAGE': bundle['images']['runtime']['name'], 'API_IMAGE': image}
                for name in ('session', 'api-rbac', 'api-quota'):
                    sessions.create(sessions.render(name, params))
                sessions.create(sessions.client_policy(client, work, run))
                alice, bob = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
                with tempfile.TemporaryDirectory(prefix='obs-private-', dir=self.lab.state_dir) as tmp:
                    api.secret(Path(tmp), client, alice, bob)
                    sessions.create(instrument(self.lab, sessions.render('api', params)))
                    api.connect(client)
                    self.attach('session', work, api.record['api_pod'], client)
                    self.wait('up{job="host"} == 1')
                    self.mark('loopback_stack_and_separate_api_metrics_targets_ready')
                    view = api.session('observation', alice, runtime, work)
                    code = "import time,hashlib\nuntil=time.monotonic()+25\nwhile time.monotonic()<until: hashlib.sha256(b'x'*65536).digest()\nprint(333833500)"
                    with ThreadPoolExecutor(max_workers=1) as pool:
                        result = pool.submit(api.execute, view, alice, code, {200}, 60)
                        self.wait('agentbox_runtime_observation_valid{node_role="worker",source="cri"} == 1')
                        self.wait('agentbox_runtime_resource{domain="session",source="cri",resource="cpu_cores",role="compute"} > 0')
                        status, value = result.result(timeout=90)
                    require(value['exit_code'] == 0 and value['stdout'].strip() == '333833500', 'session_observation_failed', 'wrong real command result')
                    api.execute(view, alice, "raise ValueError('expected program failure')", expected={200})
                    self.wait('agentbox_operations_total{mode="session",operation="execute",outcome="program_failure"} >= 1')
                    self.wait('agentbox_operations_total{mode="session",operation="execute",outcome="complete"} >= 1')
                    self.mark('real_session_runtime_resources_and_program_outcomes')
                    api.close(view, alice)
                    api.stop()
                product = ObservedProduct(self)
                self.record['product_report'] = 'batch-product-test.json'; self.save()
                product.test()
                self.record['product_checks'] = product.record['checks']
                self.mark('retry_cancel_and_published_artifact_metrics_exercised')
                dashboard = json.loads(self.stack.http(13000, '/api/dashboards/uid/agentbox-observation', True))
                expected = read_yaml(self.lab.config / 'components/observability/dashboard.yaml')
                require(dashboard['dashboard']['uid'] == expected['uid'] and len(dashboard['dashboard']['panels']) == len(expected['panels']),
                        'dashboard_source_mismatch', 'dashboard provisioning does not match reviewed source')
                for actual, reviewed in zip(dashboard['dashboard']['panels'], expected['panels']):
                    require(all(actual.get(key) == reviewed[key] for key in ('id', 'type', 'title', 'targets', 'datasource')),
                            'dashboard_source_mismatch', 'provisioned panel queries differ from reviewed source')
                self.lab.write_json(self.output / 'grafana-dashboard.json', dashboard)
                self.lab.write_json(self.output / 'prometheus-targets.json', json.loads(self.stack.http(19090, '/api/v1/targets')))
                series = {}
                for expr in ['agentbox_http_requests_total', 'agentbox_operations_total', 'agentbox_inventory_valid',
                             'agentbox_runtime_observation_valid', 'agentbox_runtime_resource', 'node_pressure_io_stalled_seconds_total']:
                    path = '/api/v1/query_range?' + urllib.parse.urlencode({'query': expr, 'start': self.record['started_at'], 'end': time.time(), 'step': 5})
                    series[expr] = json.loads(self.stack.http(19090, path))
                self.lab.write_json(self.output / 'prometheus-range.json', series)
                self.mark('provisioned_dashboard_and_raw_observations_exported')
                self.record.update(all_passed=True)
                return self.record
            finally:
                api.stop()
                self.stack.stop_observer()
                self.stack.targets({})
                for forward in self.forwards.values():
                    forward.close()
                events = self.stack.directory / 'events.jsonl'
                if events.exists():
                    shutil.copyfile(events, self.output / 'observer-events.jsonl')
                if self.namespaces:
                    sessions.close_namespaces(self.namespaces, run)
                for evidence in api.record.get('runtime_evidence', {}).values():
                    require(not runtime.processes(evidence['sandbox_id']), 'observation_cleanup_incomplete', 'session runtime remains')
                self.record['session_api'] = {key: api.record[key] for key in ('api_image', 'api_pod', 'runtime_evidence', 'sessions') if key in api.record}
                self.record['cleanup_complete'] = product is None or product.record.get('cleanup_complete', False)
                self.save()
