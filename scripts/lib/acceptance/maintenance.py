"""Owned queue/permission faults and natural expiration, without client polling."""

import copy
import json
import re
import shutil
import time

from lib.acceptance.observability import ObservationAcceptance, ObservedProduct
from lib.core.errors import BatchError
from lib.core.kubernetes import identity
from lib.core.validation import require
from lib.acceptance.retention import APIRetentionAcceptance


def wait(predicate, detail, seconds=180):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        value = predicate()
        if value:
            return value
        time.sleep(.5)
    raise BatchError('maintenance_not_converged', detail)


class MaintenanceProduct(ObservedProduct):
    def wait_task(self, view, state, seconds=180, cleanup=False):
        if state == 'succeeded' and not cleanup and not self.record.get('maintenance_fault_started'):
            self.record['maintenance_fault_started'] = True
            self.save()
            self.queue_fault(view)
        return super().wait_task(view, state, seconds, cleanup)

    def durable(self, view):
        name = view['batch_id'].split('.')[0]
        require(re.fullmatch(r'bat-[a-f0-9]{40}', name), 'invalid_fixture_id', name)
        code = "from pathlib import Path;print(Path('/data/records/" + name + ".json').read_text())"
        return json.loads(self.lab.kubectl('-n', self.record['namespace'], 'exec',
            self.record['api_pod']['name'], '-c', 'api', '--', 'python3', '-I', '-B', '-c', code,
            announce=False))

    def queue_fault(self, running):
        namespace = self.record['namespace']
        queue = self.get('localqueue', 'batch', namespace)
        role = self.get('role', 'batch-api', namespace)
        for obj in (queue, role):
            ref = next(r for r in self.record['resources'] if r['kind'] == obj['kind'] and r['name'] == obj['metadata']['name'])
            identity(obj, ref['uid'], self.record['owner'], self.record['run'])
        original_rules = role['rules']
        restricted = copy.deepcopy(original_rules)
        for rule in restricted:
            if 'jobs' in rule['resources']:
                rule['verbs'] = [v for v in rule['verbs'] if v != 'delete']

        def rules(before, after):
            patch = [{'op': 'test', 'path': '/metadata/uid', 'value': role['metadata']['uid']},
                     {'op': 'test', 'path': '/rules', 'value': before},
                     {'op': 'replace', 'path': '/rules', 'value': after}]
            self.lab.kubectl('-n', namespace, 'patch', 'role', role['metadata']['name'],
                '--type=json', '-p', json.dumps(patch), announce=False)

        def policy(value):
            patch = [{'op': 'test', 'path': '/metadata/uid', 'value': queue['metadata']['uid']},
                     {'op': 'add', 'path': '/spec/stopPolicy', 'value': value}]
            self.lab.kubectl('-n', namespace, 'patch', 'localqueue', 'batch', '--type=json',
                '-p', json.dumps(patch), announce=False)

        def active(expected):
            q = self.get('localqueue', 'batch', namespace)
            identity(q, queue['metadata']['uid'], self.record['owner'], self.record['run'])
            return any(c.get('type') == 'Active' and c.get('status') == expected
                       for c in q.get('status', {}).get('conditions', []))

        _, queued = self.api('POST', '/v2/batches', self.alice,
            {'request_id': 'maintenance-cancel', 'dataset_id': self.record['worker']['dataset']}, expected={202})
        self.capture(queued)
        require(queued['state'] == 'queued', 'maintenance_setup_failed', 'second task was not queued')
        changed_role = changed_queue = False
        try:
            rules(original_rules, restricted)
            changed_role = True
            # The intent is durable before the deliberately forbidden delete.
            self.api('DELETE', '/v2/batches/' + queued['batch_id'], self.alice, expected={503})
            require(self.durable(queued)['cancel_requested'], 'cancel_intent_missing', 'cancel was not persisted')
            policy('Hold')  # Preserve admitted work; never evict it with HoldAndDrain.
            changed_queue = True
            wait(lambda: active('False'), 'queue did not stop', 45)
            require(self.durable(running)['state'] != 'succeeded', 'maintenance_setup_too_late',
                    'result published before admission was paused')
            wait(lambda: self.api('GET', '/readyz', '', expected={200, 503})[0] == 503,
                 'readiness did not reflect inactive queue', 15)
            self.api('GET', '/healthz', '', expected={200})
            rules(restricted, original_rules)
            changed_role = False

            def background_finished():
                result = self.durable(running)
                cancelled = self.durable(queued)
                job = self.get('job', running['batch_id'].split('.')[0], namespace)
                return (result['state'] == 'succeeded' and len(result['artifacts']) == 8 and
                        cancelled['state'] == 'cancelled' and
                        job is not None and job['spec'].get('ttlSecondsAfterFinished') == 600)

            # Reading Kubernetes and the durable record cannot finish the work
            # for the server. No product task GET occurs during this interval.
            wait(background_finished, 'inactive queue blocked publication or cancel recovery')
            require(active('False'), 'queue_changed_during_fault', 'queue resumed unexpectedly')
            self.api('GET', '/readyz', '', expected={503})
            self.record['inactive_queue'] = {
                'queue_uid': queue['metadata']['uid'], 'running_task': running['batch_id'],
                'cancelled_task': queued['batch_id'], 'client_task_polling': False,
                'publication_verified': True, 'job_ttl_verified': True,
                'persisted_cancel_recovered': True, 'healthz': 200, 'readyz': 503,
            }
            self.save()
            self.harness.mark('inactive_queue_background_publication_ttl_and_cancel_recovery')
        finally:
            if changed_role:
                rules(restricted, original_rules)
            if changed_queue:
                policy(queue.get('spec', {}).get('stopPolicy', 'None'))
                wait(lambda: active('True'), 'queue did not recover', 45)
                wait(lambda: self.api('GET', '/readyz', '', expected={200, 503})[0] == 200,
                     'readiness did not recover', 15)


class MaintenanceRetention(APIRetentionAcceptance):
    def __init__(self, harness):
        super().__init__(harness.lab)
        self.harness = harness

    def mark(self, name):
        super().mark(name)
        if name == 'independent_cleanup_role_and_unchanged_product_role':
            namespace = self.record['namespaces']['work']['name']
            try:
                self.harness.stack.watch({'session': namespace})
                self.harness.wait('agentbox_inventory_items{domain="session",kind="claim",state="closing"} == 32')
                self.harness.wait('agentbox_inventory_items{domain="session",kind="claim",state="creating"} == 0')
                self.harness.wait('agentbox_inventory_items{domain="session",kind="claim",state="closed"} == 0')
                self.harness.mark('natural_claim_expiry_is_closing_before_product_confirmation')
            finally:
                self.harness.stack.stop_observer()
                events = self.harness.stack.directory / 'events.jsonl'
                if events.exists():
                    shutil.copyfile(events, self.output / 'retention-observer-events.jsonl')


class MaintenanceAcceptance(ObservationAcceptance):
    product_class = MaintenanceProduct

    def test(self):
        with self.lab.operation_lock():
            super().test()
            self.record.update(all_passed=False, cleanup_complete=False)
            self.save()
            retention = MaintenanceRetention(self)
            retention.test()
            self.record['retention_report'] = 'api-retention-test.json'
            self.record['retention_checks'] = retention.record['checks']
            self.record.update(all_passed=True, cleanup_complete=retention.record['cleanup_complete'])
            self.save()
            return self.record
