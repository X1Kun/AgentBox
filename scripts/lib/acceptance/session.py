"""SDK session acceptance and acceptance-only client image preparation."""

from lib.platform.sandbox import checked_source
from lib.core.paths import ROOT as PROJECT_ROOT
from lib.manifests import render_upstream, FIXTURES, acceptance_definition
import hashlib
import json
from pathlib import Path
import re
import shutil
import tempfile
import time
import uuid

from lib.platform.gvisor import digest
from lib.platform.calico import OWNER

CONTROLLER_NS = 'agent-sandbox-system'
RUN = 'agentbox.dev/session-run'
ROLE = 'agentbox.dev/session-role'

# Acceptance source changes do not change the installed upstream runtime.
RUNTIME_BUILD_INPUTS = ('go.mod', 'go.sum', 'deploy/platform/session.lock.json',
                        'deploy/platform/sandbox-runtime.Dockerfile')
EXPECTED_CHECKS = {
    'client_rbac_scope', 'multi_call_file_persistence', 'cross_session_files_isolated',
    'program_failure_does_not_end_session', 'fresh_python_interpreter_per_command',
    'nonroot_readonly_no_cluster_token', 'file_api_workspace_boundary',
    'peer_and_cluster_api_egress_blocked', 'stable_claim_sandbox_pod_service_identity',
    'explicit_close_and_reject_new_calls', 'controller_claim_expiry_and_gc',
    'deadline_no_replay_and_session_cleanup', 'close_running_parent_and_child',
}
WARM_CHECKS = {
    'client_rbac_scope', 'fixed_two_ready_members_before_claims',
    'concurrent_claims_keep_distinct_precreated_pods', 'parallel_warm_sessions_keep_files_isolated',
    'pool_replenishes_within_total_pod_budget', 'exhaustion_is_bounded_without_creation_replay',
    'used_sessions_deleted_and_closed_handles_rejected', 'capacity_recovers_with_clean_new_environment',
    'fixed_inventory_restored_without_used_members',
}
FAULT_CHECKS = {
    'client_rbac_scope', 'controller_restart_keeps_session_identity_and_workspace',
    'inflight_command_survives_controller_restart_once',
    'pod_replacement_changes_uid_and_invalidates_old_identity',
    'replacement_environment_has_no_implicit_workspace_restore',
    'changed_session_closed_without_command_replay',
    'explicit_new_claim_gets_clean_executable_environment',
    'expiry_and_close_converge_with_idempotent_close',
}


from lib.platform.sandbox import Sessions
from lib.core.errors import SessionError

class SessionAcceptance:
    def __init__(self, lab, platform=None):
        self.platform = platform or Sessions(lab)
        self.lab = lab
        self.output = lab.runner.output

    def current_client_bundle(self, installed):
        """Build the current acceptance command without rebinding a live runtime.

        The installation bundle stays immutable. Only this run's client image,
        build directory and archive use the current source fingerprint.
        """
        inputs = self.client_inputs()
        if inputs == installed['inputs'] and 'client' in installed['images']:
            return installed
        if any(inputs.get(key) != installed['inputs'].get(key) for key in RUNTIME_BUILD_INPUTS):
            raise SessionError('session_bundle_changed', 'runtime inputs changed')
        version = self.lab.runner.run(['go', 'version']).strip()
        if version != installed['go_version'] or self.platform.lock['go_toolchain'] not in version:
            raise SessionError('go_toolchain_mismatch', 'use the installed pinned toolchain')
        fingerprint = hashlib.sha256(json.dumps(inputs, sort_keys=True).encode()).hexdigest()
        directory = self.lab.state_dir / ('session-client-build-' + fingerprint[:16])
        directory.mkdir(parents=True, exist_ok=True)
        binary = directory / 'agentbox-session-smoke'
        self.lab.runner.run(['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH=amd64',
                             'go', 'build', '-p=2', '-mod=readonly', '-trimpath', '-buildvcs=false',
                             '-ldflags=-s -w', '-o', binary, './cmd/agentbox-session-smoke'], timeout=900)
        base = checked_source(self.lab.lock['smoke']['image'], installed['python_source'])
        if base not in self.platform.docker_image(base).get('RepoDigests', []):
            raise SessionError('session_base_image_changed', 'cached base digest differs')
        image = 'agentbox/session-client:' + fingerprint[:16]
        with tempfile.TemporaryDirectory(prefix='session-client-image-', dir=self.lab.state_dir) as temporary:
            context = Path(temporary)
            shutil.copyfile(FIXTURES / 'sessions/client.Dockerfile', context / 'Dockerfile')
            shutil.copy2(binary, context / 'agentbox-session-smoke')
            self.lab.runner.run([self.lab.tools['docker'], 'build', '--pull=false', '--network=none',
                                 '--platform=linux/amd64', '--build-arg', 'PYTHON_IMAGE=' + base,
                                 '--label', 'agentbox.dev/build=' + fingerprint, '-t', image, context], timeout=300)
        client = {'name': image, 'id': self.platform.docker_image(image)['Id']}
        checksum = digest(binary)
        result = dict(installed, fingerprint=fingerprint, inputs=inputs,
                      images=dict(installed['images'], client=client),
                      binary_sha256=dict(installed.get('binary_sha256', {}),
                                         **{'agentbox-session-smoke': checksum}))
        self.platform.save('session-client-build.json', {
            'fingerprint': fingerprint, 'installed_fingerprint': installed['fingerprint'],
            'inputs': inputs, 'image': client, 'binary_sha256': checksum,
            'go_version': version, 'runtime_reused': installed['images']['runtime'],
        })
        # Import only the acceptance image; do not unpack unchanged runtime assets.
        self.platform.load_images(dict(result, images={'client': client}))
        return result


    def observer(self, namespace, run_id, image):
        params = {'NAME': 'observer', 'NAMESPACE': namespace, 'OWNER': self.platform.state['session'],
                  'RUN': run_id, 'ROLE': 'observer', 'IMAGE': image}
        obj = self.platform.render('observer', params)
        spec = obj['spec']
        spec['containers'][0].update(command=['/usr/local/bin/python3', '-I', '-B', '-u', '-c',
                                       (self.lab.root / 'tests/fixtures/probes/network_server.py').read_text()])
        value = self.platform.create(obj)[0]
        self.lab.kubectl('-n', namespace, 'wait', '--for=condition=Ready', 'pod/observer', '--timeout=120s', timeout=140)
        return {'namespace': namespace, 'name': 'observer', 'uid': value['metadata']['uid']}


    def inspect_identity(self, identity, namespace, run_id, runtime):
        values = {}
        for resource, name, uid in [('sandboxclaim', identity['claim'], identity['claim_uid']),
                                    ('sandbox', identity['sandbox'], identity['sandbox_uid']),
                                    ('pod', identity['pod'], identity['pod_uid']),
                                    ('service', identity['service'], identity['service_uid'])]:
            obj = json.loads(self.lab.kubectl('-n', namespace, 'get', resource, name, '-o', 'json'))
            if obj['metadata']['uid'] != uid:
                raise SessionError('session_identity_changed', name)
            values[resource] = obj
        pod = values['pod']
        if (pod['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session'] or
                pod['metadata']['labels'].get(RUN) != run_id or pod['spec'].get('runtimeClassName') != 'agentbox-gvisor' or
                pod['spec'].get('automountServiceAccountToken', True) or pod['status'].get('phase') != 'Running'):
            raise SessionError('session_pod_boundary_changed', identity['pod'])
        return runtime.runtime_evidence(pod, memory_bytes=512 * 1024 * 1024)


    def outsider_blocked(self, observer, identities, run_id, record):
        program = (self.lab.root / 'tests/fixtures/probes/network_client.py').read_text()
        cases = [{'id': i['pod_uid'] + ':' + str(port), 'protocol': 'tcp', 'ip': i['pod_ip'], 'port': port}
                 for i in identities for port in (8080, 9090)]
        until = time.monotonic() + 45
        consecutive = 0
        samples = []
        while time.monotonic() < until:
            output = json.loads(self.lab.kubectl('-n', observer['namespace'], 'exec', observer['name'], '-c', 'compute',
                                  '--', '/usr/local/bin/python3', '-I', '-B', '-c', program, json.dumps(cases),
                                  timeout=15, announce=False))
            if output.get('pod_uid') != observer['uid'] or output.get('run') != run_id:
                raise SessionError('observer_identity_changed', 'untrusted observer was replaced')
            if len(output['results']) != len(cases) or {v['id'] for v in output['results']} != {c['id'] for c in cases}:
                raise SessionError('invalid_observer_response', 'incomplete session ingress probes')
            samples.append(output['results'])
            consecutive = consecutive + 1 if all(v['kind'] == 'connect_timeout' for v in output['results']) else 0
            if consecutive >= 2:
                record['outsider_ingress'] = samples
                return
            time.sleep(.5)
        record['outsider_ingress'] = samples
        raise SessionError('session_ingress_not_isolated', 'untrusted namespace can reach the session runtime')


    def warm_quota_ready(self, namespace):
        until = time.monotonic() + 60
        while time.monotonic() < until:
            quota = json.loads(self.lab.kubectl('-n', namespace, 'get', 'resourcequota', 'session-budget', '-o', 'json', announce=False))
            if quota.get('status', {}).get('hard') == quota['spec']['hard']:
                return quota
            time.sleep(.5)
        raise SessionError('warm_quota_not_ready', 'ResourceQuota has not observed the fixed budget')


    def inspect_supply(self, supply, pool_uid, namespace, run_id, runtime, record):
        for identity in supply:
            sandbox = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandbox', identity['sandbox'], '-o', 'json'))
            pod = json.loads(self.lab.kubectl('-n', namespace, 'get', 'pod', identity['pod'], '-o', 'json'))
            if (sandbox['metadata']['uid'] != identity['sandbox_uid'] or pod['metadata']['uid'] != identity['pod_uid'] or
                    not any(r.get('uid') == pool_uid and r.get('controller') for r in sandbox['metadata'].get('ownerReferences', [])) or
                    not any(r.get('uid') == identity['sandbox_uid'] and r.get('controller') for r in pod['metadata'].get('ownerReferences', [])) or
                    pod['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session'] or
                    pod['metadata']['labels'].get(RUN) != run_id or pod['spec'].get('automountServiceAccountToken', True)):
                raise SessionError('warm_supply_identity_changed', identity['pod'])
            record['runtime_evidence'][identity['pod_uid']] = runtime.runtime_evidence(pod, memory_bytes=512 * 1024 * 1024)


    def observe_exhaustion(self, namespace, run_id, used, record):
        claims = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandboxclaims', '-o', 'json', announce=False))['items']
        remaining = [c for c in claims if c['metadata']['uid'] not in {v['claim_uid'] for v in used}]
        if len(remaining) > 1:
            raise SessionError('warm_attempt_replayed', 'more than one extra Claim exists')
        if not remaining:
            return
        claim = remaining[0]
        if claim['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session'] or claim['metadata']['labels'].get(RUN) != run_id:
            raise SessionError('unowned_warm_attempt', claim['metadata']['name'])
        previous = record.get('exhausted_claim')
        identity = {'name': claim['metadata']['name'], 'uid': claim['metadata']['uid']}
        if previous and previous != identity:
            raise SessionError('warm_attempt_replayed', 'extra Claim identity changed')
        record['exhausted_claim'] = identity
        objects = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandboxes', '-o', 'json', announce=False))['items']
        for sandbox in objects:
            if not any(r.get('uid') == identity['uid'] and r.get('controller') for r in sandbox['metadata'].get('ownerReferences', [])):
                continue
            for condition in sandbox.get('status', {}).get('conditions', []):
                if (condition.get('type') == 'Ready' and condition.get('status') == 'False' and
                        condition.get('observedGeneration') == sandbox['metadata']['generation'] and
                        'exceeded quota: session-budget' in condition.get('message', '') and 'pods' in condition['message']):
                    record['quota_rejection'] = {'claim': identity, 'sandbox': {'name': sandbox['metadata']['name'], 'uid': sandbox['metadata']['uid']},
                                                 'condition': condition}


    def verify_exhaustion_rollback(self, namespace, event, record):
        proof = record.get('quota_rejection')
        if not proof:
            raise SessionError('warm_quota_rejection_missing', 'a timeout alone does not prove quota exhaustion')
        quota = json.loads(self.lab.kubectl('-n', namespace, 'get', 'resourcequota', 'session-budget', '-o', 'json'))
        if (quota['metadata']['uid'] != event['warm']['quota_uid'] or quota['spec']['hard']['pods'] != '3' or
                quota.get('status', {}).get('used', {}).get('pods') != '3'):
            raise SessionError('warm_budget_changed', 'expected three active Pods at exhaustion')
        until = time.monotonic() + 90
        while time.monotonic() < until:
            remaining = []
            for kind in ('sandboxclaim', 'sandbox'):
                identity = proof['claim' if kind == 'sandboxclaim' else 'sandbox']
                output = self.lab.kubectl('-n', namespace, 'get', kind, identity['name'], '--ignore-not-found', '-o', 'json', announce=False)
                if output.strip():
                    obj = json.loads(output)
                    if obj['metadata']['uid'] != identity['uid']:
                        raise SessionError('warm_attempt_identity_changed', identity['name'])
                    remaining.append(kind)
            for kind in ('pods', 'services'):
                objects = json.loads(self.lab.kubectl('-n', namespace, 'get', kind, '-o', 'json', announce=False))['items']
                remaining += [kind for obj in objects if any(r.get('uid') == proof['sandbox']['uid'] for r in obj['metadata'].get('ownerReferences', []))]
            if not remaining:
                record['failed_open_cleanup_confirmed'] = True
                record['quota_at_exhaustion'] = quota
                return
            time.sleep(.5)
        raise SessionError('warm_attempt_cleanup_incomplete', 'failed Open left Claim-owned resources')


    def stop_warm_pool(self, namespace, event):
        pool = json.loads(self.lab.kubectl('-n', namespace, 'get', 'sandboxwarmpool', 'python-warm', '-o', 'json'))
        if pool['metadata']['uid'] != event['warm']['pool_uid'] or pool['metadata'].get('labels', {}).get(OWNER) != self.platform.state['session']:
            raise SessionError('unowned_warm_pool', 'pool identity changed before cleanup')
        self.lab.kubectl('-n', namespace, 'delete', 'sandboxwarmpool', 'python-warm', '--cascade=foreground', '--wait=true', '--timeout=90s', timeout=110)
        for kind in ('sandboxclaims', 'sandboxes', 'pods', 'services'):
            values = json.loads(self.lab.kubectl('-n', namespace, 'get', kind, '-o', 'json'))['items']
            if values:
                raise SessionError('warm_pool_cleanup_incomplete', kind + ' remain after pool shutdown')


    def send_marker(self, namespace, pod_name, marker, record):
        try:
            self.lab.kubectl('-n', namespace, 'exec', pod_name, '-c', 'client', '--',
                             '/usr/local/bin/python3', '-I', '-B', '-c',
                             "from pathlib import Path; Path('/work/" + marker + "').touch()")
        except Exception as error:
            # The final marker lets PID 1 exit, which can kill this exec before
            # its status is delivered. Do not replay it. The caller still must
            # observe the matching complete event and successful Job, then
            # independently verify all runtime and namespace cleanup.
            commands = self.lab.runner.commands
            code = commands[-1].get('exit_code') if commands else None
            if marker not in ('warm-finish', 'fault-finish') or getattr(error, 'reason', None) != 'command_failed' or code != 137:
                raise
            record['final_marker_transport_error'] = {'exit_code': code, 'detail': str(error)}


    def test(self, profile="cold"):
        if profile not in ('cold', 'warm', 'fault'):
            raise SessionError('invalid_session_profile', profile)
        with self.lab.operation_lock():
            runtime = self.platform.attach()
            bundle = self.platform.bundle(require_client=False)
            self.platform.verify(bundle)
            bundle = self.current_client_bundle(bundle)
            run_id = uuid.uuid4().hex[:10]
            namespaces = {}
            record = {'profile': profile, 'run': run_id, 'owner': self.platform.state['session'], 'namespaces': namespaces,
                      'runtime_evidence': {}, 'go_events': [], 'cleanup_complete': False}
            record['acceptance_definition'] = acceptance_definition(self.lab.config)
            try:
                roles = [('work', 'agentbox-session-' + run_id), ('client', 'agentbox-session-client-' + run_id)]
                if profile == 'cold':
                    roles.append(('observer', 'agentbox-session-observer-' + run_id))
                for role, name in roles:
                    namespaces[role] = self.platform.namespace(name, role, run_id)
                params = {'OWNER': self.platform.state['session'], 'RUN': run_id, 'WORK_NS': namespaces['work']['name'],
                          'CLIENT_NS': namespaces['client']['name'], 'RUNTIME_IMAGE': bundle['images']['runtime']['name'],
                          'CLIENT_IMAGE': bundle['images']['client']['name']}
                template = self.platform.render('session-warm' if profile == 'warm' else 'session', params)
                if profile == 'warm':
                    quota = self.platform.render('warm-quota', params)
                    self.platform.create(quota)
                    record['quota_initial'] = self.warm_quota_ready(params['WORK_NS'])
                self.platform.save('session-template.json', template)
                created = self.platform.create(template)
                template_uid = next(o['metadata']['uid'] for o in created if o['kind'] == 'SandboxTemplate')
                self.platform.create(self.platform.render('sdk-rbac', params))
                self.platform.create(self.platform.client_policy(params['CLIENT_NS'], params['WORK_NS'], run_id))
                self.platform.policies_ready(params['WORK_NS'], template_uid)
                faults = None
                if profile == 'fault':
                    from lib.acceptance.session_faults import FaultActions
                    faults = FaultActions(self, runtime, params['WORK_NS'], run_id, bundle['images']['controller']['name'])
                observer = self.observer(namespaces['observer']['name'], run_id, bundle['images']['client']['name']) if profile == 'cold' else None
                job_spec = self.platform.render('sdk-client', params)
                job_spec['spec']['template']['spec']['containers'][0]['env'].append({'name': 'SESSION_PROFILE', 'value': profile})
                client_job = self.platform.create(job_spec)[0]
                job_uid = client_job['metadata']['uid']
                record.update(client_job_uid=job_uid, template_uid=template_uid, observer=observer)
                until = time.monotonic() + 800
                handled = set()
                client_pod = None
                complete = None
                while time.monotonic() < until:
                    if profile == 'warm' and 'warm-claimed' in handled and 'warm-exhausted' not in handled:
                        self.observe_exhaustion(params['WORK_NS'], run_id, record['warm_claimed'], record)
                    job = json.loads(self.lab.kubectl('-n', params['CLIENT_NS'], 'get', 'job', 'session-client', '-o', 'json', announce=False))
                    if job['metadata']['uid'] != job_uid:
                        raise SessionError('session_client_identity_changed', 'client Job was replaced')
                    pods = json.loads(self.lab.kubectl('-n', params['CLIENT_NS'], 'get', 'pods', '-l', 'job-name=session-client', '-o', 'json', announce=False))['items']
                    if len(pods) > 1:
                        raise SessionError('unexpected_client_attempts', 'trusted SDK client must not be replayed')
                    if pods:
                        pod = pods[0]
                        if not any(r.get('uid') == job_uid and r.get('controller') for r in pod['metadata'].get('ownerReferences', [])):
                            raise SessionError('session_client_identity_changed', 'Pod is not owned by this Job')
                        if client_pod and client_pod['metadata']['uid'] != pod['metadata']['uid']:
                            raise SessionError('session_client_identity_changed', 'client Pod was replaced')
                        client_pod = pod
                        states = pod.get('status', {}).get('containerStatuses', [])
                        if states and ('running' in states[0].get('state', {}) or 'terminated' in states[0].get('state', {})):
                            logs = self.lab.kubectl('-n', params['CLIENT_NS'], 'logs', pod['metadata']['name'], '-c', 'client',
                                                    '--limit-bytes=65536', announce=False)
                            events = [json.loads(line) for line in logs.splitlines() if line.strip()]
                            record['go_events'] = events
                            (self.output / 'go-client.jsonl').write_text(logs)
                            for event in events:
                                if (event.get('run') != run_id or event.get('owner') != self.platform.state['session'] or
                                        event.get('namespace') != params['WORK_NS'] or event.get('sdk_version') != self.platform.lock['version'] or
                                        event.get('profile') != profile):
                                    raise SessionError('invalid_session_event', 'Go client identity differs')
                                stage = event['stage']
                                if stage in handled:
                                    continue
                                if stage == 'failed':
                                    raise SessionError('go_session_acceptance_failed', event.get('error', 'unknown error'))
                                marker = None
                                if profile == 'fault' and stage.startswith('fault-'):
                                    marker = faults.handle(event, record)
                                elif profile == 'warm' and stage == 'warm-prepared':
                                    self.inspect_supply(event['warm']['initial'], event['warm']['pool_uid'], params['WORK_NS'], run_id, runtime, record)
                                    marker = 'warm-start'
                                elif profile == 'warm' and stage == 'warm-claimed':
                                    record['warm_claimed'] = event['sessions']
                                    identity = event['sessions'][2]
                                    record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    marker = 'warm-exhaust'
                                elif profile == 'warm' and stage == 'warm-exhausted':
                                    self.platform.verify_exhaustion_rollback(params['WORK_NS'], event, record)
                                    marker = 'warm-release'
                                elif profile == 'warm' and stage == 'warm-reused':
                                    identity = event['sessions'][3]
                                    record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    marker = 'warm-return'
                                elif profile == 'warm' and stage == 'warm-recovered':
                                    self.inspect_supply(event['warm']['recovered'], event['warm']['pool_uid'], params['WORK_NS'], run_id, runtime, record)
                                    marker = 'warm-finish'
                                elif stage == 'ready':
                                    for identity in event['sessions']:
                                        record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    self.outsider_blocked(observer, event['sessions'], run_id, record)
                                    marker = 'proceed'
                                elif stage == 'timeout-ready':
                                    identity = event['timed_out']
                                    record['runtime_evidence'][identity['pod_uid']] = self.inspect_identity(identity, params['WORK_NS'], run_id, runtime)
                                    marker = 'timeout'
                                elif stage == 'closing-active':
                                    self.inspect_identity(event['sessions'][0], params['WORK_NS'], run_id, runtime)
                                    marker = 'close'
                                elif stage == 'complete':
                                    complete = event
                                if marker:
                                    print('[platform] ' + profile + ' session checkpoint: ' + stage, flush=True)
                                    self.send_marker(params['CLIENT_NS'], pod['metadata']['name'], marker, record)
                                handled.add(stage)
                    self.platform.save('session-test.json', record)
                    conditions = job.get('status', {}).get('conditions', [])
                    if any(c['type'] == 'Failed' and c['status'] == 'True' for c in conditions):
                        raise SessionError('session_client_job_failed', json.dumps(job['status']))
                    if any(c['type'] == 'Complete' and c['status'] == 'True' for c in conditions):
                        expected = {'cold': EXPECTED_CHECKS, 'warm': WARM_CHECKS, 'fault': FAULT_CHECKS}[profile]
                        required = {'warm': {'warm-prepared', 'warm-claimed', 'warm-exhausted', 'warm-reused', 'warm-recovered'},
                                    'cold': {'ready', 'timeout-ready', 'closing-active'},
                                    'fault': {'fault-controller', 'fault-pod-loss', 'fault-replacement', 'fault-expiry-close', 'fault-finished'}}[profile]
                        if (complete is None or not complete.get('cleanup_complete') or
                                {c['name'] for c in complete.get('checks', [])} != expected or
                                not all(c['passed'] for c in complete['checks']) or
                                not required.issubset(handled)):
                            raise SessionError('incomplete_session_acceptance', 'client completed without full evidence')
                        break
                    time.sleep(1)
                else:
                    raise SessionError('session_acceptance_timeout', 'trusted SDK client did not complete')
                identities = list(complete['sessions'])
                if profile == 'warm':
                    self.stop_warm_pool(params['WORK_NS'], complete)
                    record['pool_cleanup_complete'] = True
                    identities += complete['warm']['initial'] + complete['warm']['recovered']
                elif profile == 'cold':
                    identities += [complete['expiring'], complete['timed_out']]
                deadline = time.monotonic() + 60
                while time.monotonic() < deadline:
                    processes = [p for evidence in record['runtime_evidence'].values()
                                 for p in runtime.processes(evidence['sandbox_id'])]
                    cri = json.loads(runtime.node('crictl', 'pods', '-o', 'json'))['items']
                    cri = [s for s in cri if s.get('labels', {}).get('io.kubernetes.pod.uid') in {i['pod_uid'] for i in identities}]
                    if not processes and not cri:
                        record['runtime_cleanup'] = {'remaining_processes': [], 'remaining_cri_sandboxes': []}
                        break
                    time.sleep(1)
                else:
                    raise SessionError('session_runtime_cleanup_incomplete', 'gVisor process or CRI sandbox remains')
                self.platform.close_namespaces(namespaces, run_id)
                record.update(cleanup_complete=True, all_passed=True, checks=complete['checks'])
                return record
            finally:
                self.platform.save('session-test.json', record)


    def client_inputs(self):
        inputs = self.platform.build_inputs(runtime_only=True)
        for path in sorted((self.lab.root/'cmd/agentbox-session-smoke').glob('*.go')):
            if not path.name.endswith('_test.go'):
                inputs[str(path.relative_to(self.lab.root))] = digest(path)
        inputs['deploy/platform/session-client.Dockerfile'] = digest(FIXTURES/'sessions/client.Dockerfile')
        return inputs
