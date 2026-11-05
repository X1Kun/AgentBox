"""gVisor installation and trusted lifecycle acceptance in one owned kind worker."""

from lib.manifests import render as render_manifest
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import tarfile
import time
import uuid

RUNTIME = 'agentbox-gvisor'
INSTALL = '/opt/agentbox-gvisor'
CONFIG = '/etc/containerd/config.toml'
RUNSC_CONFIG = '/etc/containerd/agentbox-runsc.toml'
ROOT = '/run/containerd/runsc/k8s.io'
OWNER = 'agentbox.dev/session'
CASES = ('success', 'failure', 'cancel', 'deadline', 'oom', 'missing')


from lib.platform.gvisor import GVisor
from lib.core.errors import GVisorError

class GVisorAcceptance:
    def __init__(self, lab, runtime=None):
        self.runtime = runtime or GVisor(lab)
        self.lab = lab
        self.output = lab.runner.output

    def job(self, mode, image, runtime=RUNTIME):
        name = 'agentbox-gvisor-' + mode + '-' + uuid.uuid4().hex[:8]
        obj = render_manifest(self.lab.config, 'gvisor-probe', {'NAME': name, 'OWNER': self.runtime.state['session'], 'IMAGE': image})
        pod = obj['spec']['template']['spec']
        pod['runtimeClassName'] = runtime
        source = (self.lab.root / 'tests/fixtures/probes/gvisor_guest_probe.py').read_text()
        pod['containers'][0]['command'] = ['/usr/local/bin/python3', '-I', '-B', '-u', '-c', source,
                                          'success' if mode == 'missing' else mode]
        if mode == 'deadline':
            obj['spec']['activeDeadlineSeconds'] = 45
        self.runtime.save(name + '-manifest.json', obj)
        created = json.loads(self.lab.kubectl('-n', 'agentbox-platform', 'create', '-f', '-', '-o', 'json',
                                            input_text=json.dumps(obj)))
        return {'name': name, 'uid': created['metadata']['uid'], 'mode': mode,
                'runtime_class': runtime, 'image': image, 'cleanup_complete': False}


    def pods(self, record):
        value = json.loads(self.lab.kubectl('-n', 'agentbox-platform', 'get', 'pods', '-l',
                                           'job-name=' + record['name'], '-o', 'json', announce=False))['items']
        for pod in value:
            containers = pod['spec'].get('containers', [])
            if (pod['metadata'].get('labels', {}).get(OWNER) != self.runtime.state['session'] or
                    pod['spec'].get('runtimeClassName') != record['runtime_class'] or
                    len(containers) != 1 or containers[0]['image'] != record['image'] or
                    not any(o.get('uid') == record['uid'] and o.get('controller') and o.get('kind') == 'Job'
                            for o in pod['metadata'].get('ownerReferences', []))):
                raise GVisorError('probe_identity_changed', 'probe Pod differs from the submitted Job')
        if len(value) > 1:
            raise GVisorError('unexpected_attempts', 'probe must have exactly one attempt')
        return value


    def get_job(self, record):
        value = json.loads(self.lab.kubectl('-n', 'agentbox-platform', 'get', 'job', record['name'],
                                           '-o', 'json', announce=False))
        if value['metadata']['uid'] != record['uid']:
            raise GVisorError('probe_identity_changed', 'Job was replaced')
        return value


    def logs(self, pod):
        return self.lab.kubectl('-n', 'agentbox-platform', 'logs', pod['metadata']['name'],
                                '-c', 'compute', '--limit-bytes=8192', announce=False)


    def wait_ready(self, record, timeout):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            values = self.pods(record)
            if values:
                pod = values[0]
                if pod['status'].get('phase') in ('Failed', 'Succeeded'):
                    raise GVisorError('probe_ended_early', json.dumps(pod['status']))
                statuses = pod['status'].get('containerStatuses', [])
                if statuses and statuses[0].get('state', {}).get('running'):
                    logs = self.logs(pod)
                    if logs.strip():
                        try:
                            ready = json.loads(logs.splitlines()[0])
                        except ValueError as error:
                            raise GVisorError('invalid_probe_output', logs) from error
                        if ready != {'ready': True, 'uid': 1000, 'readonly_root': True,
                                     'serviceaccount_token': False, 'mode': record['mode']}:
                            raise GVisorError('invalid_probe_output', logs)
                        record.update(pod_name=pod['metadata']['name'], pod_uid=pod['metadata']['uid'],
                                      ready=ready, runtime=self.runtime.runtime_evidence(pod))
                        return pod
            time.sleep(1)
        raise GVisorError('probe_start_timeout', record['name'])


    def cleanup(self, record):
        self.get_job(record)  # Verify identity immediately before deletion.
        self.lab.kubectl('-n', 'agentbox-platform', 'delete', 'job', record['name'],
                         '--cascade=foreground', '--wait=true', '--timeout=60s', timeout=80)
        until = time.monotonic() + 60
        sandbox = record.get('runtime', {}).get('sandbox_id')
        while time.monotonic() < until:
            remaining = self.pods(record)
            live = self.runtime.processes(sandbox) if sandbox else []
            cri = json.loads(self.runtime.node('crictl', 'pods', '-o', 'json'))['items']
            cri = [s for s in cri if s.get('labels', {}).get('io.kubernetes.pod.uid') == record.get('pod_uid')]
            if not remaining and not live and not cri:
                record['cleanup_complete'] = True
                return
            time.sleep(1)
        raise GVisorError('gvisor_cleanup_incomplete', 'Pod, runtime process or CRI sandbox still exists')


    def memory_snapshot(self, record):
        uid = record['pod_uid']
        groups = [r['cgroup'] for r in record['runtime']['resources']
                  if uid.replace('-', '_') in r['cgroup'] or uid in r['cgroup']]
        group = min(groups, key=len)
        return self.runtime.node('sh', '-c', 'for f in memory.max memory.current memory.peak memory.swap.max '
                         'memory.swap.current memory.events cgroup.procs; do '
                         'if [ -f "$1/$f" ]; then printf "%s\\n" "$f"; cat "$1/$f"; fi; done',
                         'probe', '/sys/fs/cgroup' + group)


    def wait_terminal(self, record, timeout):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            if record['mode'] == 'oom':
                record.setdefault('memory_observations', []).append(self.memory_snapshot(record))
            job = self.get_job(record)
            values = self.pods(record)
            if record['mode'] == 'deadline' and any(
                    c.get('reason') == 'DeadlineExceeded' and c['status'] == 'True' and c['type'] == 'Failed'
                    for c in job.get('status', {}).get('conditions', [])) and not values:
                # The Job controller deletes active Pods when the deadline expires.
                # Do not invent an exit code; cleanup separately verifies runtime disappearance.
                record.update(job_status=job['status'], termination=None, logs=None,
                              terminal_evidence='DeadlineExceeded; Pod removed by Job controller')
                return
            if values and any(c['status'] == 'True' and c['type'] in ('Complete', 'Failed')
                              for c in job.get('status', {}).get('conditions', [])):
                pod = values[0]
                if pod['metadata']['uid'] != record['pod_uid']:
                    raise GVisorError('probe_identity_changed', 'Pod was replaced')
                term = pod['status'].get('containerStatuses', [{}])[0].get('state', {}).get('terminated', {})
                record.update(job_status=job['status'], termination=term, logs=self.logs(pod))
                mode = record['mode']
                if mode == 'success':
                    valid = term.get('exitCode') == 0 and json.loads(record['logs'].splitlines()[-1]) == {'result': 333833500}
                elif mode == 'failure':
                    valid = term.get('exitCode') == 23
                elif mode == 'oom':
                    baseline = max([0] + [int(n) for r in record.get('runtime', {}).get('resources', [])
                                           if record['pod_uid'].replace('-', '_') in r.get('cgroup', '') or
                                           record['pod_uid'] in r.get('cgroup', '')
                                           for n in re.findall(r'^oom_kill (\d+)$', r['limits'], re.M)])
                    observed = max([0] + [int(n) for value in record.get('memory_observations', [])
                                           for n in re.findall(r'^oom_kill (\d+)$', value, re.M)])
                    record['oom_kill_delta'] = max(0, observed - baseline)
                    valid = (term.get('exitCode') not in (None, 0) and
                             (term.get('reason') == 'OOMKilled' or record['oom_kill_delta'] > 0))
                else:
                    valid = any(c.get('reason') == 'DeadlineExceeded' and c['status'] == 'True'
                                for c in job['status'].get('conditions', []))
                if not valid:
                    raise GVisorError('unexpected_probe_result', json.dumps(record))
                return
            time.sleep(1)
        raise GVisorError('probe_terminal_timeout', record['name'])


    def missing_handler(self, image, timeout):
        name = 'agentbox-missing-' + uuid.uuid4().hex[:8]
        rc = self.runtime.ensure_runtime_class(self.runtime.runtime_class(name, name))
        record = self.job('missing', image, name)
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            values = self.pods(record)
            if values:
                pod = values[0]
                record.update(pod_name=pod['metadata']['name'], pod_uid=pod['metadata']['uid'])
                statuses = pod['status'].get('containerStatuses', [])
                if any(s.get('state', {}).get(k) for s in statuses for k in ('running', 'terminated')):
                    raise GVisorError('runtime_fallback', 'unregistered handler unexpectedly executed a container')
                events = json.loads(self.lab.kubectl('-n', 'agentbox-platform', 'get', 'events',
                                      '--field-selector', 'involvedObject.uid=' + record['pod_uid'],
                                      '-o', 'json', announce=False))['items']
                messages = [e.get('message', '') for e in events if e.get('reason') == 'FailedCreatePodSandBox']
                if any(name in m and ('not configured' in m or 'no runtime' in m) for m in messages):
                    record['expected_failure_events'] = messages
                    self.cleanup(record)
                    self.lab.kubectl('delete', 'runtimeclass', rc['metadata']['name'], '--wait=true')
                    return record
            time.sleep(1)
        raise GVisorError('missing_handler_not_observed', record['name'])


    def test(self, image, timeout, cases=None):
        cases = tuple(cases or CASES)
        results = []
        with self.lab.operation_lock():
            self.runtime.attach()
            self.runtime.verify_installation()
            self.lab.namespace(self.runtime.state)
            try:
                for mode in (mode for mode in cases if mode != 'missing'):
                    print('[platform] gVisor lifecycle probe: ' + mode, flush=True)
                    record = self.job(mode, image)
                    results.append(record)
                    pod = self.wait_ready(record, timeout)
                    self.runtime.save('gvisor-test.json', results)
                    if mode != 'cancel':
                        self.lab.kubectl('-n', 'agentbox-platform', 'exec', pod['metadata']['name'], '-c', 'compute',
                                         '--', '/usr/local/bin/python3', '-I', '-B', '-c',
                                         "from pathlib import Path; Path('/work/go').touch()")
                        self.wait_terminal(record, timeout)
                    self.cleanup(record)
                if 'missing' in cases:
                    print('[platform] gVisor lifecycle probe: missing handler', flush=True)
                    results.append(self.missing_handler(image, timeout))
                return {'cases': results, 'requested_cases': cases, 'all_passed': True, 'scope': 'trusted runtime lifecycle only; no NetworkPolicy acceptance'}
            finally:
                self.runtime.save('gvisor-test.json', results)


