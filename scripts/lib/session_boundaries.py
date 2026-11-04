"""Bounded behavioral experiments with fixed arrivals and independently checked results."""
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import uuid

from lib.boundary_workloads import compile_workload
from lib.experiment_diagnostics import archive_observer, archive_metrics
from lib.platform_api_execution import APIExecutionAcceptance
from lib.platform_batch import require
from lib.platform_manifests import read_yaml
from lib.platform_sessions import Sessions
from lib.observability_acceptance import ObservationAcceptance, instrument


class SessionBoundaries(ObservationAcceptance):
    config_path = "acceptance/boundaries/session.yaml"
    def test(self):
        with self.lab.operation_lock():
            sessions = Sessions(self.lab)
            runtime = sessions.attach()
            bundle = sessions.bundle(require_client=False)
            sessions.verify(bundle)
            run = uuid.uuid4().hex[:10]
            self.record.update(run=run, owner=sessions.state['session'], started_at=time.time(),
                               scope='R behavioral boundaries; no statistically established capacity')
            self.save()
            config = read_yaml(self.lab.config / self.config_path)
            selected = getattr(self, 'selected_scenarios', None)
            if selected:
                require(set(selected) <= set(config['scenarios']), 'invalid_scenario_selection', str(selected))
                config['scenarios'] = {k: v for k, v in config['scenarios'].items() if k in selected}
                self.record['selected_scenarios'] = selected
            api = APIExecutionAcceptance(self.lab)
            api.record.update(runtime_evidence={}, owner=sessions.state['session'], run=run)
            captured = {}
            try:
                self.stack.up()
                image = api.build_image(sessions)
                for role in ('work', 'client'):
                    self.namespaces[role] = sessions.namespace('agentbox-boundary-' + role + '-' + run, 'boundary-' + role, run)
                self.record['namespaces'] = self.namespaces
                self.save()
                work, client = self.namespaces['work']['name'], self.namespaces['client']['name']
                params = {'OWNER': sessions.state['session'], 'RUN': run, 'WORK_NS': work,
                          'CLIENT_NS': client, 'RUNTIME_IMAGE': bundle['images']['runtime']['name'], 'API_IMAGE': image}
                for name in ('session-warm', 'api-rbac', 'api-quota'):
                    resource = sessions.render(name, params)
                    if name == 'api-quota' and 'pod_budget' in config:
                        n = config['pod_budget']
                        require(isinstance(n, int) and 3 <= n <= 8, 'invalid_pod_budget', str(n))
                        resource['spec']['hard'].update(pods=str(n), **{'requests.cpu': str(n*100)+'m', 'requests.memory': str(n*128)+'Mi', 'limits.cpu': str(n*500)+'m', 'limits.memory': str(n*512)+'Mi'})
                    sessions.create(resource)
                sessions.create(sessions.client_policy(client, work, run))
                binary = self.lab.state_dir / 'agentbox-session-experiment'
                self.lab.runner.run(['go', 'build', '-trimpath', '-buildvcs=false', '-o', binary,
                                     './cmd/agentbox-session-experiment'], timeout=900)
                with tempfile.TemporaryDirectory(prefix='boundary-private-', dir=self.lab.state_dir) as tmp:
                    tmp = Path(tmp)
                    alice, bob = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
                    api.secret(tmp, client, alice, bob)
                    credentials = {'alice': alice, 'bob': bob}
                    users_path = tmp / 'users.json'
                    self.lab.write_json(users_path, credentials)
                    users_path.chmod(0o600)
                    self.record['experiment_config'] = config
                    deployment = instrument(self.lab, sessions.render('api', params))
                    args = deployment['spec']['template']['spec']['containers'][0]['args']
                    args[args.index('--pool=python-cold')] = '--pool=python-warm'
                    sessions.create(deployment)
                    api.connect(client)
                    self.attach('session', work, api.record['api_pod'], client)
                    self.wait('agentbox_inventory_items{domain="session",kind="pool",state="ready"} == 2')
                    self.mark('fixed_two_ready_members_before_trace')
                    for name, scenario in config['scenarios'].items():
                        self.record['current_scenario'] = name
                        self.save()
                        waiting = time.monotonic()
                        self.wait('agentbox_inventory_items{domain="session",kind="pool",state="ready"} == 2')
                        self.record.setdefault('pool_recovery_seconds', {})[name] = time.monotonic() - waiting
                        owners = scenario.get('users', ['alice', 'alice'])
                        require(2 <= len(owners) <= 4 and all(u in credentials for u in owners), 'invalid_users', name)
                        targets = [api.session(name + '-' + str(i), credentials[user], runtime, work) for i, user in enumerate(owners)]
                        ready_at = time.time()
                        if len(set(owners)) > 1:
                            api.api('GET', '/v2/sessions/' + targets[0]['session_id'], credentials['bob' if owners[0] == 'alice' else 'alice'], expected={404})
                            api.api('POST', '/v2/sessions/' + targets[0]['session_id'] + '/execute', credentials['bob' if owners[0] == 'alice' else 'alice'], {'identity_revision': targets[0]['identity_revision'], 'code': 'print(999)'}, expected={404})
                            self.mark(name + '_cross_principal_get_execute_denied')
                        captured.update(api.record['runtime_evidence'])
                        for i, view in enumerate(targets):
                            api.file(view, credentials[owners[i]], 'marker', (run + ':' + str(i)).encode())
                        if name == 'code-shapes':
                            verified = []
                            for kind in ('memory', 'files', 'output', 'failure'):
                                index = len(verified) % len(targets)
                                code, expected, exit_code = compile_workload(kind, run + ':' + str(index))
                                _, result = api.execute(targets[index], credentials[owners[index]], code, expected={200}, seconds=20)
                                require(result['stdout'] == expected and result['exit_code'] == exit_code, 'workload_contract_failed', kind)
                                verified.append(kind)
                            self.record['isolated_workload_contracts'] = verified
                            self.mark('real_sandbox_memory_files_output_failure_contracts')
                        if name in ('sequential', 'burst'):
                            extras = []
                            for i in range(config.get('pod_budget', 3) - len(targets) + 1):
                                key = 'excess-' + name + '-' + str(i)
                                status, v = api.api('POST', '/v2/sessions', alice, {'request_id': key}, {200, 202, 503, 504})
                                self.record.setdefault('burst_offered', []).append({'request_id': key, 'http_status': status, 'response': v})
                                self.save()
                                if status in (200, 202):
                                    extras.append(v)
                                else:
                                    # Resolve only an existing same-name Claim for cleanup;
                                    # never resubmit the failed burst arrival to create work.
                                    claim_name = 'ses-' + hashlib.sha256(('api-' + run + '\0alice\0' + key).encode()).hexdigest()[:40]
                                    text = self.lab.kubectl('-n', work, 'get', 'sandboxclaim', claim_name, '--ignore-not-found', '-o', 'json', announce=False)
                                    if text.strip():
                                        claim = json.loads(text)
                                        require(claim['metadata']['labels'].get('agentbox.dev/api-owner') == hashlib.sha256(b'alice').hexdigest()[:32], 'foreign_burst_claim', key)
                                        require(claim['metadata']['labels'].get('agentbox.dev/api-instance') == 'api-' + run, 'foreign_burst_instance', key)
                                        extras.append({'session_id': claim_name + '.' + claim['metadata']['uid']})
                            time.sleep(2)
                            statuses = [api.api('GET', '/v2/sessions/' + v['session_id'], alice, expected={200, 503, 504})[1] for v in extras]
                            require(any(v.get('state') != 'ready' for v in statuses) or any(x['http_status'] >= 500 for x in self.record.get('burst_offered', [])), 'pool_not_exhausted', 'burst never exceeded available pool')
                            self.record['burst'] = statuses
                            for view in extras:
                                api.close(view, alice)
                            self.mark('burst_pool_exhaustion_is_bounded_and_closed')
                        requests = []
                        for item in scenario['requests']:
                            item = dict(item)
                            kind = item.pop('work', 'short')
                            index = item['target']
                            code, expected, exit_code = compile_workload(kind, run + ':' + str(index))
                            item.update(action=item.get('action', 'execute'), code=code,
                                        timeout_seconds=item.get('timeout_seconds', 20),
                                        expected_out=expected, expected_exit=exit_code)
                            requests.append(item)
                        trace = dict(targets=[{'id': t['session_id'], 'revision': t['identity_revision'], 'user': owners[i]} for i, t in enumerate(targets)],
                                     requests=requests, max_inflight=scenario['max_inflight'],
                                     max_lag_ms=config['max_lag_ms'], deadline_ms=config['deadline_ms'])
                        path = tmp / 'trace.json'
                        self.lab.write_json(path, trace)
                        self.lab.write_json(self.output / (name + '-trace.json'), trace)
                        ledger = self.output / (name + '.jsonl')
                        result = subprocess.run([binary, '--config', path, '--ledger', ledger,
                            '--api-url', api.base, '--api-ca-file', tmp / 'tls.crt', '--users-file', users_path],
                            env=dict(os.environ, AGENTBOX_API_TOKEN=alice), capture_output=True, text=True, timeout=210)
                        require(result.returncode == 0, 'experiment_driver_failed', name + ': ' + result.stderr[-300:])
                        summary = json.loads(result.stdout)
                        self.lab.write_json(self.output / (name + '-summary.json'), summary)
                        events = [json.loads(line) for line in ledger.read_text().splitlines()]
                        require(len(events) == len(requests) and len({e['id'] for e in events}) == len(events), 'ledger_unbalanced', name)
                        require(summary['measurement_valid'], 'generator_invalid', name)
                        require(not summary['outcomes'].get('wrong'), 'wrong_result', name)
                        per_user = {}
                        for user in set(owners):
                            user_events = [e for e in events if e['user'] == user]
                            latencies = sorted(e['finished_ms'] - e['sent_ms'] for e in user_events if e['sent_ms'] >= 0)
                            per_user[user] = {'planned': len(user_events), 'sent': sum(e['sent_ms'] >= 0 for e in user_events), 'correct': sum(e['outcome'] == 'correct' for e in user_events), 'timely_correct': sum(e['timely'] for e in user_events), 'p95_all_sent_ms': latencies[(len(latencies)*95-1)//100] if latencies else None}
                        never_called = [t['session_id'] for t in targets if not any(e['session_id'] == t['session_id'] for e in events)]
                        assessment = {}
                        if scenario.get('users'):
                            assessment['all_users_correct'] = all(any(e['user'] == user and e['outcome'] == 'correct' for e in events) for user in set(owners))
                        if name == 'sequential':
                            assessment['all_offered_commands_succeeded'] = summary['correct'] == len(requests)
                        if name in ('mixed', 'step'):
                            assessment['conflict_exercised'] = summary['outcomes'].get('rejected', 0) > 0
                            assessment['recovery_target_met'] = next(e for e in events if e['id'] == requests[-1]['id'])['outcome'] == 'correct'
                            assessment['independent_peer_succeeded'] = any(e['session_id'] == targets[1]['session_id'] and e['outcome'] == 'correct' for e in events)
                        if name == 'disconnect':
                            assessment = {'cutoff_is_unknown': any(e['id'] == 'client-cutoff' and e['outcome'] == 'unknown' for e in events), 'peer_succeeded': any(e['id'] == 'unaffected-peer' and e['outcome'] == 'correct' for e in events)}
                        if name == 'deadline':
                            assessment['deadline_exercised'] = summary['outcomes'].get('unknown', 0) == 1
                        if name == 'close-race':
                            assessment['close_interrupted_execution'] = any(e['outcome'] in ('unknown', 'error') for e in events if e['action'] == 'execute')
                        self.record.setdefault('scenarios', {})[name] = dict(per_user=per_user, no_commands_during_trace=never_called, ready_at=ready_at, session_users=owners, pod_count=len(targets), summary=summary, assessment=assessment, quality_target_met=all(assessment.values()), trace_sha256=hashlib.sha256(path.read_bytes()).hexdigest(), trace=trace)
                        self.save()
                        for i, view in enumerate(targets):
                            api.close(view, credentials[owners[i]])
                        self.record['scenarios'][name]['closed_at'] = time.time()
                        self.mark(name + '_result_ledger_and_cleanup')
                    # Collector loss must not stop execution or invent empty inventory.
                    loss_view = api.session('observation-loss', alice, runtime, work)
                    captured.update(api.record['runtime_evidence'])
                    self.stack.stop_observer()
                    self.wait('up{job="observer"} == 0')
                    self.wait('absent(agentbox_inventory_items{domain="session"})')
                    _, result = api.execute(loss_view, alice, 'print(6*7)', expected={200})
                    require(result['stdout'] == '42\n', 'observation_loss_stopped_business', run)
                    api.close(loss_view, alice)
                    self.stack.watch(self.scope)
                    self.wait('agentbox_inventory_valid{domain="session"} == 1')
                    self.mark('observation_loss_and_recovery_are_explicit')
                    self.lab.write_json(self.output / 'prometheus-targets.json', json.loads(self.stack.http(19090, '/api/v1/targets')))
                    import urllib.parse
                    series = {}
                    for expr in ('agentbox_http_requests_total', 'agentbox_operations_total', 'agentbox_diagnostics_total', 'agentbox_diagnostic_duration_seconds_bucket', 'agentbox_inventory_valid', 'agentbox_runtime_resource', 'node_pressure_io_stalled_seconds_total'):
                        query = urllib.parse.urlencode({'query': expr, 'start': self.record['started_at'], 'end': time.time(), 'step': 5})
                        series[expr] = json.loads(self.stack.http(19090, '/api/v1/query_range?' + query))
                    self.lab.write_json(self.output / 'prometheus-range.json', series)
                self.record['all_passed'] = True
                return self.record
            finally:
                import sys
                primary = sys.exc_info()[1]
                if primary is not None:
                    self.record['primary_failure'] = {'type': type(primary).__name__, 'reason': getattr(primary, 'reason', None)}
                self._teardown(api, sessions, runtime, captured, run)

    def _capture_diagnostics(self, api):
        if self.namespaces and not self.record['all_passed']:
            snapshots = {}
            for resource in ('sandboxclaims', 'sandboxes', 'sandboxwarmpools', 'pods', 'resourcequota'):
                try:
                    objects = json.loads(self.lab.kubectl('-n', self.namespaces['work']['name'], 'get', resource, '-o', 'json', announce=False))['items']
                    snapshots[resource] = [{'metadata': {k: o['metadata'].get(k) for k in ('name', 'uid', 'generation', 'ownerReferences')}, 'status': o.get('status', {})} for o in objects]
                except Exception as error:
                    snapshots[resource] = {'unavailable': type(error).__name__}
            self.lab.write_json(self.output / 'failure-snapshots.json', snapshots)
        if self.namespaces and api.record.get('api_pod'):
            try:
                logs = self.lab.kubectl('-n', self.namespaces['client']['name'], 'logs',
                    api.record['api_pod']['name'], '-c', 'api', '--tail=10000', announce=False)
                (self.output / 'api-events.jsonl').write_text(logs)
                events = json.loads(self.lab.kubectl('-n', self.namespaces['work']['name'],
                    'get', 'events', '-o', 'json', announce=False))
                self.lab.write_json(self.output / 'kubernetes-events.json', events)
                namespace = self.namespaces['work']['name']
                controller_logs = self.lab.kubectl('-n', 'agent-sandbox-system', 'logs',
                    'deployment/agent-sandbox-controller', '--tail=10000', announce=False)
                selected = [line for line in controller_logs.splitlines() if namespace in line]
                (self.output / 'controller-events.log').write_text('\n'.join(selected) + '\n')
            except Exception as error:
                self.record['diagnostic_capture_error'] = type(error).__name__

    def _collect_cleanup_evidence(self, runtime, captured):
        pods = json.loads(self.lab.kubectl('-n', self.namespaces['work']['name'], 'get', 'pods', '-o', 'json', announce=False))['items']
        for pod in pods:
            if pod['spec'].get('runtimeClassName') == 'agentbox-gvisor' and pod.get('status', {}).get('phase') == 'Running':
                evidence = runtime.runtime_evidence(pod, memory_bytes=512*1024*1024)
                captured[pod['metadata']['uid']] = evidence
        sandboxes = json.loads(runtime.node('crictl', 'pods', '-o', 'json'))['items']
        extra_ids = [s['id'] for s in sandboxes if s.get('metadata', {}).get('namespace') == self.namespaces['work']['name']]
        self.record['cleanup_sandbox_ids'] = extra_ids
        self.record['runtime_evidence'] = captured

    def _capture_metrics(self):
        if self.record.get('started_at'):
            archive_metrics(self.stack, self.output/'prometheus-final.json', self.record['started_at'], time.time())

    def _archive_observer(self):
        self.record['observer_archive'] = archive_observer(self.stack.directory/'events.jsonl',
            self.output/'observer-events.jsonl', self.record.get('started_at', 0), time.time())

    def _teardown(self, api, sessions, runtime, captured, run):
        """Evidence collection is fallible; namespace deletion must still run.

        Keep teardown errors separate from the experiment's primary failure.
        Unknown runtime evidence must not become a successful cleanup report.
        """
        errors = self.record.setdefault('teardown_errors', [])
        self.record['cleanup_complete'] = False
        self.record['namespace_cleanup_complete'] = False
        self.record['runtime_cleanup_complete'] = False

        def attempt(stage, operation, resource=None):
            before = time.time()
            tick = time.monotonic()
            outcome = 'complete'
            try:
                operation()
                return True
            except Exception as error:
                outcome = 'error'
                # Error text may contain credentials or native response data.
                errors.append({'stage': stage, 'type': type(error).__name__,
                               'reason': getattr(error, 'reason', None)})
                return False
            finally:
                self.record.setdefault('teardown_timeline', []).append({'stage': stage, 'resource': resource,
                    'started_at': before, 'ended_at': time.time(), 'duration_seconds': time.monotonic()-tick, 'outcome': outcome})

        evidence_ok = not self.namespaces
        try:
            attempt('diagnostic_archive', lambda: self._capture_diagnostics(api))
            attempt('metrics_archive', self._capture_metrics)
            attempt('api_forward_stop', api.stop)
            attempt('observer_stop', self.stack.stop_observer)
            attempt('observer_archive', self._archive_observer)
            attempt('scrape_targets_clear', lambda: self.stack.targets({}))
            for index, forward in enumerate(self.forwards.values()):
                attempt('metrics_forward_stop_' + str(index), forward.close)
            if self.namespaces:
                evidence_ok = attempt('runtime_evidence', lambda: self._collect_cleanup_evidence(runtime, captured))
        finally:
            # Deliberately nested: no failed log write, observer stop, Pod list,
            # CRI query or evidence inspection may bypass scoped namespace GC.
            try:
                if self.namespaces:
                    namespaces_ok = True
                    for role, namespace in self.namespaces.items():
                        namespaces_ok = attempt(
                            'namespace_delete_' + role,
                            lambda role=role, namespace=namespace: sessions.close_namespaces({role: namespace}, run), namespace
                        ) and namespaces_ok
                    self.record['namespace_cleanup_complete'] = namespaces_ok
                else:
                    self.record['namespace_cleanup_complete'] = True
            finally:
                self.record['runtime_evidence'] = captured
                self.record['remaining_processes'] = {}
                ids = {v['sandbox_id'] for v in captured.values()}
                ids.update(self.record.get('cleanup_sandbox_ids', []))
                process_checks_ok = True
                for sid in ids:
                    def inspect(sid=sid):
                        self.record['remaining_processes'][sid] = runtime.processes(sid)
                    process_checks_ok = attempt('runtime_process_check', inspect, {'sandbox_id': sid}) and process_checks_ok
                self.record['runtime_cleanup_complete'] = (
                    evidence_ok and process_checks_ok and
                    all(not value for value in self.record['remaining_processes'].values()))
                self.record['cleanup_complete'] = (
                    self.record['namespace_cleanup_complete'] and
                    self.record['runtime_cleanup_complete'] and not errors)
                if not self.record['cleanup_complete']:
                    self.record['all_passed'] = False
                self.save()
        require(self.record['cleanup_complete'], 'boundary_teardown_incomplete',
                'teardown errors or unconfirmed runtime cleanup; inspect report')


class SessionMatrix(SessionBoundaries):
    config_path = 'acceptance/boundaries/matrix.yaml'
