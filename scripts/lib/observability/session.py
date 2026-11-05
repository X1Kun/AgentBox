import json
import time
from lib.core.validation import require
from lib.observability.diagnostics import archive_observer, archive_metrics
from lib.observability.environment import ObservationEnvironment

class SessionEnvironment(ObservationEnvironment):
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


