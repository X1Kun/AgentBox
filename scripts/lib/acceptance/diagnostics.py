"""Intentionally fail one owned observer domain; verify correlation and archival."""

from lib.core.errors import ToolError
import json

from lib.core.kubernetes import identity
from lib.core.validation import require
from lib.experiments.static_baseline import StaticBaseline


def batch_fault_isolated(issues):
    return 'batch_inventory_unknown' in issues and set(issues) <= {'batch_inventory_unknown', 'batch_inventory_stale'}


class DiagnosticsAcceptance(StaticBaseline):
    def restore_observer_role(self, product, namespace, ref, original):
        current = product.get('role', ref['name'], namespace)
        identity(current, ref['uid'], self.record['owner'])
        rules = current.get('rules')
        require(not rules or rules == original, 'observer_role_changed', 'do not overwrite changed rules')
        if rules == original:
            return
        # RBAC serializes an empty rule set as null (or omits it). Check the
        # actual representation plus resourceVersion before restoring it.
        operations = [{'op': 'test', 'path': '/metadata/uid', 'value': ref['uid']},
                      {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']}]
        if 'rules' in current:
            operations.append({'op': 'test', 'path': '/rules', 'value': rules})
        operations.append({'op': 'add', 'path': '/rules', 'value': original})
        self.lab.kubectl('-n', namespace, 'patch', 'role', ref['name'], '--type=json',
                         '-p', json.dumps(operations), announce=False)

    def cell(self, cell, config, targets, api, token, product, users_path, private, binary):
        namespace = product.record['namespace']
        role = product.get('role', 'agentbox-observer', namespace)
        state = self.stack.state()
        ref = next(r for r in state['rbac'] if r['kind'] == 'Role' and
                   r.get('namespace') == namespace and r['name'] == 'agentbox-observer')
        identity(role, ref['uid'], self.record['owner'])
        original = role['rules']
        self.record['fault_injection'] = {'kind': 'observer_batch_permission_denied',
            'role': ref, 'expected_baseline_failure': 'baseline_observation_invalid'}
        self.save()

        def patch(expected, replacement):
            operations = [{'op': 'test', 'path': '/metadata/uid', 'value': ref['uid']},
                          {'op': 'test', 'path': '/rules', 'value': expected},
                          {'op': 'replace', 'path': '/rules', 'value': replacement}]
            self.lab.kubectl('-n', namespace, 'patch', 'role', ref['name'], '--type=json',
                             '-p', json.dumps(operations), announce=False)

        try:
            patch(original, [])
            self.wait('agentbox_inventory_requests_total{domain="batch",reason="forbidden"} >= 1')
            self.wait('agentbox_inventory_valid{domain="batch"} == 0')
            self.wait('agentbox_inventory_valid{domain="session"} == 1')
            super().cell(cell, config, targets, api, token, product, users_path, private, binary)
        finally:
            self.restore_observer_role(product, namespace, ref, original)
            self.wait('agentbox_inventory_valid{domain="batch"} == 1')

    def test(self):
        expected = None
        try:
            super().test(self.lab.root/'experiments/configs/session/diagnostics.yaml')
        except ToolError as error:
            expected = error.reason
        require(expected == 'baseline_observation_invalid', 'unexpected_diagnostic_outcome', str(expected))
        require(self.record['cleanup_complete'], 'diagnostic_cleanup_incomplete', 'see static-baseline.json')
        require(self.record['observer_archive']['status'] == 'archived' and self.record['observer_archive']['lines'] > 0,
                'missing_failure_archive', 'observer logs were not frozen')
        require(self.record['diagnostics_sampler']['status'] == 'stopped', 'diagnostic_sampler_incomplete', 'see raw samples')
        cell = self.record['cells'][0]
        require(cell['session']['measurement_valid'] and batch_fault_isolated(cell['observation']['issues']),
                'observer_fault_crossed_domains', 'healthy session observation must remain valid')
        directory = self.output/cell['id']
        events = [json.loads(line) for line in (directory/'session.jsonl').read_text().splitlines()]
        logs = [json.loads(line) for line in (self.output/'api-events.jsonl').read_text().splitlines()]
        required = {'runtime_validate_before', 'runtime_connect', 'runtime_validate_after', 'runtime_rpc_start',
                    'runtime_wait_init', 'runtime_first_output', 'runtime_process', 'runtime_stream_finish'}
        linked = []
        for event in events:
            if event['outcome'] != 'correct':
                continue
            stages = {line['stage'] for line in logs if line.get('request_id') == event['request_id'] and
                      line.get('msg') == 'sandboxd stage' and line.get('phase') == 'end'}
            require(required <= stages, 'incomplete_runtime_timeline', event['request_id'])
            require(any(line.get('request_id') == event['request_id'] and line.get('stage') == 'claim_get'
                        for line in logs), 'missing_backend_correlation', event['request_id'])
            linked.append({'request_id': event['request_id'], 'stages': sorted(stages)})
        require(linked, 'no_correct_diagnostic_command', 'see request ledger')
        observer = [json.loads(line) for line in (self.output/'observer-events.jsonl').read_text().splitlines()]
        failures = [line for line in observer if line.get('msg') == 'inventory read' and
                    line.get('domain') == 'batch' and line.get('reason') == 'forbidden']
        require(failures and all(line.get('poll_id') and line.get('resource') and line.get('http_status') == 403
                                for line in failures), 'incomplete_observer_diagnostics', 'missing classified read failures')
        control = [json.loads(line) for line in (self.output/'controlplane-diagnostics.jsonl').read_text().splitlines()]
        components = {row['component'] for row in control if row.get('status') == 'sampled'}
        require(components == {'apiserver', 'etcd'}, 'missing_controlplane_evidence', str(components))
        cleanup_samples = [json.loads(line) for line in (self.output/'cleanup-resources.jsonl').read_text().splitlines()]
        require(any('namespaces' in row for row in cleanup_samples), 'missing_cleanup_timeline', 'no scoped cleanup snapshot')
        result = {'diagnostic_acceptance_passed': True, 'expected_baseline_failure': expected,
                  'cleanup_complete': True, 'qualified_baseline': False, 'runtime_requests': linked,
                  'observer_forbidden_reads': len(failures), 'controlplane_components': sorted(components),
                  'cleanup_snapshots': len(cleanup_samples), 'scope': self.record['scope']}
        self.lab.write_json(self.output/'diagnostics-acceptance.json', result)
        return result
