"""UID-fenced fault injection for the trusted upstream session acceptance only."""
from datetime import datetime, timedelta, timezone
import json
import time

from lib.platform_sessions import CONTROLLER_NS, OWNER, RUN, SessionError


def owned_reference(obj, uid):
    return any(v.get('uid') == uid and v.get('controller') is True
               for v in obj['metadata'].get('ownerReferences', []))


def require_identity(obj, uid, owner=None, run_id=None):
    labels = obj['metadata'].get('labels', {})
    if (obj['metadata']['uid'] != uid or (owner is not None and labels.get(OWNER) != owner) or
            (run_id is not None and labels.get(RUN) != run_id)):
        raise SessionError('fault_target_identity_changed', obj['metadata']['name'])


class FaultActions:
    def __init__(self, sessions, runtime, namespace, run_id, controller_image):
        self.s = sessions
        self.lab = sessions.lab
        self.runtime = runtime
        self.namespace = namespace
        self.run_id = run_id
        self.owner = sessions.state['session']
        self.controller_image = controller_image

    def get(self, namespace, kind, name):
        value = self.lab.kubectl('-n', namespace, 'get', kind, name, '--ignore-not-found', '-o', 'json', announce=False)
        return json.loads(value) if value.strip() else None

    def delete_pod(self, namespace, pod):
        # UID preconditions fence replacement objects even between GET and DELETE.
        options = {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': pod['metadata']['uid']},
                   'propagationPolicy': 'Background'}
        path = '/api/v1/namespaces/' + namespace + '/pods/' + pod['metadata']['name']
        self.lab.kubectl('delete', '--raw', path, '-f', '-', input_text=json.dumps(options))

    def controller_pods(self, deployment):
        selector = ','.join(k + '=' + v for k, v in sorted(deployment['spec']['selector']['matchLabels'].items()))
        return json.loads(self.lab.kubectl('-n', CONTROLLER_NS, 'get', 'pods', '-l', selector, '-o', 'json', announce=False))['items']

    def require_controller_pod(self, pod, deployment_uid):
        references = [v for v in pod['metadata'].get('ownerReferences', []) if v.get('controller')]
        if len(references) != 1 or references[0]['kind'] != 'ReplicaSet':
            raise SessionError('unowned_fault_controller_pod', pod['metadata']['name'])
        replica = self.get(CONTROLLER_NS, 'replicaset', references[0]['name'])
        if (replica is None or replica['metadata']['uid'] != references[0]['uid'] or
                not owned_reference(replica, deployment_uid) or
                pod['spec']['containers'][0]['image'] != self.controller_image):
            raise SessionError('unowned_fault_controller_pod', pod['metadata']['name'])

    def restart_controller(self, record):
        uid = self.s.state['sessions']['controller_uid']
        deployment = self.get(CONTROLLER_NS, 'deployment', 'agent-sandbox-controller')
        if deployment is None:
            raise SessionError('fault_controller_missing', 'installed Deployment disappeared')
        require_identity(deployment, uid, self.owner)
        pods = self.controller_pods(deployment)
        if len(pods) != 1 or pods[0]['metadata'].get('deletionTimestamp'):
            raise SessionError('fault_controller_not_stable', 'expected one non-terminating upstream controller Pod')
        old = pods[0]
        self.require_controller_pod(old, uid)
        self.delete_pod(CONTROLLER_NS, old)
        until = time.monotonic() + 90
        while time.monotonic() < until:
            current = self.get(CONTROLLER_NS, 'deployment', 'agent-sandbox-controller')
            if current is None:
                raise SessionError('fault_controller_missing', 'Deployment disappeared during restart')
            require_identity(current, uid, self.owner)
            for pod in self.controller_pods(current):
                if pod['metadata']['uid'] == old['metadata']['uid'] or pod['metadata'].get('deletionTimestamp'):
                    continue
                if not any(v['type'] == 'Ready' and v['status'] == 'True' for v in pod.get('status', {}).get('conditions', [])):
                    continue
                self.require_controller_pod(pod, uid)
                remaining = self.get(CONTROLLER_NS, 'pod', old['metadata']['name'])
                if remaining is not None:
                    require_identity(remaining, old['metadata']['uid'])
                    continue
                record['controller_restart'] = {'deployment_uid': uid,
                    'old_pod': {'name': old['metadata']['name'], 'uid': old['metadata']['uid']},
                    'new_pod': {'name': pod['metadata']['name'], 'uid': pod['metadata']['uid']}}
                return
            time.sleep(.5)
        raise SessionError('fault_controller_restart_timeout', 'no Ready replacement controller after the old Pod disappeared')

    def pod_loss(self, identity, record):
        self.s.inspect_identity(identity, self.namespace, self.run_id, self.runtime)
        pod = self.get(self.namespace, 'pod', identity['pod'])
        if pod is None:
            raise SessionError('fault_pod_missing_before_injection', identity['pod'])
        require_identity(pod, identity['pod_uid'], self.owner, self.run_id)
        if not owned_reference(pod, identity['sandbox_uid']):
            raise SessionError('unowned_fault_session_pod', identity['pod'])
        self.delete_pod(self.namespace, pod)
        until = time.monotonic() + 150
        while time.monotonic() < until:
            sandbox = self.get(self.namespace, 'sandbox', identity['sandbox'])
            claim = self.get(self.namespace, 'sandboxclaim', identity['claim'])
            if sandbox is None or claim is None:
                raise SessionError('fault_session_disappeared', 'expected the same Claim and Sandbox during Pod replacement')
            require_identity(sandbox, identity['sandbox_uid'])
            require_identity(claim, identity['claim_uid'], self.owner, self.run_id)
            pods = json.loads(self.lab.kubectl('-n', self.namespace, 'get', 'pods', '-o', 'json', announce=False))['items']
            candidates = [v for v in pods if owned_reference(v, identity['sandbox_uid']) and
                          v['metadata']['uid'] != identity['pod_uid'] and not v['metadata'].get('deletionTimestamp')]
            if len(candidates) > 1:
                raise SessionError('fault_duplicate_backing_pods', 'multiple replacement Pods')
            if candidates and any(v['type'] == 'Ready' and v['status'] == 'True'
                                  for v in candidates[0].get('status', {}).get('conditions', [])):
                new = candidates[0]
                require_identity(new, new['metadata']['uid'], self.owner, self.run_id)
                probe = dict(identity, pod=new['metadata']['name'], pod_uid=new['metadata']['uid'], pod_ip=new['status']['podIP'])
                record['runtime_evidence'][probe['pod_uid']] = self.s.inspect_identity(probe, self.namespace, self.run_id, self.runtime)
                record['pod_loss'] = {'old_pod_uid': identity['pod_uid'], 'new_pod_uid': probe['pod_uid'],
                                      'claim_uid': identity['claim_uid'], 'sandbox_uid': identity['sandbox_uid']}
                return
            time.sleep(.5)
        raise SessionError('fault_pod_replacement_timeout', 'no Ready Pod replacement for the same Sandbox')

    def expire_claim(self, identity, record):
        record['runtime_evidence'][identity['pod_uid']] = self.s.inspect_identity(identity, self.namespace, self.run_id, self.runtime)
        claim = self.get(self.namespace, 'sandboxclaim', identity['claim'])
        if claim is None:
            raise SessionError('fault_claim_expired_before_injection', identity['claim'])
        require_identity(claim, identity['claim_uid'], self.owner, self.run_id)
        when = (datetime.now(timezone.utc) + timedelta(seconds=10)).replace(microsecond=0).isoformat().replace('+00:00', 'Z')
        patch = [{'op': 'test', 'path': '/metadata/uid', 'value': identity['claim_uid']},
                 {'op': 'replace', 'path': '/spec/lifecycle/shutdownTime', 'value': when}]
        updated = json.loads(self.lab.kubectl('-n', self.namespace, 'patch', 'sandboxclaim', identity['claim'],
                                            '--type=json', '-p', json.dumps(patch), '-o', 'json'))
        require_identity(updated, identity['claim_uid'], self.owner, self.run_id)
        record['expiry_close'] = {'claim_uid': identity['claim_uid'], 'shutdown_time': when}

    def handle(self, event, record):
        stage = event['stage']
        if stage == 'fault-controller':
            identity = event['fault']['original']
            record['runtime_evidence'][identity['pod_uid']] = self.s.inspect_identity(identity, self.namespace, self.run_id, self.runtime)
            self.restart_controller(record)
            return 'fault-controller-done'
        if stage == 'fault-pod-loss':
            self.pod_loss(event['fault']['original'], record)
            return 'fault-pod-replaced'
        if stage == 'fault-replacement':
            identity = event['fault']['replacement']
            if identity['pod_uid'] != record['pod_loss']['new_pod_uid']:
                raise SessionError('fault_replacement_identity_changed', 'SDK observed a different replacement')
            self.s.inspect_identity(identity, self.namespace, self.run_id, self.runtime)
            return 'fault-pod-inspected'
        if stage == 'fault-expiry-close':
            self.expire_claim(event['fault']['expiring'], record)
            return 'fault-expiry-armed'
        if stage == 'fault-finished':
            return 'fault-finish'
        return None
