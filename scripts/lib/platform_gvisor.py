"""gVisor installation and trusted lifecycle acceptance in one owned kind worker."""
import bz2
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import tarfile
import tempfile
import shutil
import time
import uuid

RUNTIME = 'agentbox-gvisor'
INSTALL = '/opt/agentbox-gvisor'
CONFIG = '/etc/containerd/config.toml'
RUNSC_CONFIG = '/etc/containerd/agentbox-runsc.toml'
ROOT = '/run/containerd/runsc/k8s.io'
OWNER = 'agentbox.dev/session'
CASES = ('success', 'failure', 'cancel', 'deadline', 'oom', 'missing')


class GVisorError(Exception):
    def __init__(self, reason, detail):
        super().__init__(detail)
        self.reason = reason


def digest(path, algorithm='sha256'):
    h = hashlib.new(algorithm)
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def checked_archive(path, lock):
    if digest(path, 'sha512') != lock['sha512']:
        raise GVisorError('gvisor_checksum_mismatch', 'archive differs from the official pinned checksum')
    files = {}
    with tarfile.open(path, 'r:bz2') as archive:
        for member in archive:
            name = PurePosixPath(member.name)
            if name.is_absolute() or '..' in name.parts or not (member.isfile() or member.isdir()):
                raise GVisorError('invalid_gvisor_archive', 'archive contains a link or unsafe path')
            if member.isdir():
                if str(name) != 'gvisor-bin':
                    raise GVisorError('invalid_gvisor_archive', 'unexpected archive directory')
                continue
            if str(name) not in lock['required_files'] or str(name) in files:
                raise GVisorError('invalid_gvisor_archive', 'unexpected or duplicate archive file')
            h = hashlib.sha256()
            with archive.extractfile(member) as stream:
                for chunk in iter(lambda: stream.read(1 << 20), b''):
                    h.update(chunk)
            files[str(name)] = h.hexdigest()
    if set(files) != set(lock['required_files']):
        raise GVisorError('invalid_gvisor_archive', 'complete gVisor distribution is required')
    return files


def runtime_config(original):
    version = re.search(r'^version\s*=\s*(\d+)\s*$', original, re.M)
    if not version or int(version[1]) not in (2, 3, 4) or RUNTIME in original:
        raise GVisorError('unsupported_containerd_config', 'expected an unmodified supported kind config')
    plugin = 'io.containerd.grpc.v1.cri' if version[1] == '2' else 'io.containerd.cri.v1.runtime'
    # Preserve the node's schema/default runtime; containerd handles its own migration.
    return original + f'''\n# AgentBox owned lab runtime; ordinary runc remains the default.
[plugins."{plugin}".containerd.runtimes.{RUNTIME}]
  runtime_type = "io.containerd.runsc.v1"
  runtime_path = "{INSTALL}/containerd-shim-runsc-v1"
[plugins."{plugin}".containerd.runtimes.{RUNTIME}.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "{RUNSC_CONFIG}"
'''


class GVisor:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.lock = json.loads((lab.config / 'gvisor.lock.json').read_text())
        if (self.lock.get('schema_version') != 1 or self.lock.get('platform') != 'systrap' or
                not re.fullmatch(r'[a-f0-9]{128}', self.lock.get('sha512', '')) or
                self.lock.get('swap_policy', {}).get('unit') != 'kubelet.slice' or
                self.lock.get('swap_policy', {}).get('MemorySwapMax') != 0):
            raise GVisorError('invalid_gvisor_lock', 'invalid fixed runtime definition')
        self.worker_name = 'agentbox-platform-lab-worker'
        self.worker = None
        self.state = None

    def save(self, name, value):
        (self.output / name).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')

    def attach(self):
        self.state, nodes = self.lab.owned(complete=True)
        observed = self.lab.node_report()
        if any(n['node_info']['containerRuntimeVersion'] != 'containerd://' + self.lock['containerd_version']
               for n in observed):
            raise GVisorError('containerd_version_mismatch', 'gVisor profile requires the locked containerd')
        self.worker = self.lab.runner.run([self.lab.tools['docker'], 'inspect', self.worker_name,
                                          '--format', '{{.Id}}']).strip()
        if not re.fullmatch(r'[a-f0-9]{64}', self.worker) or self.worker not in nodes:
            raise GVisorError('node_identity_changed', 'worker does not belong to the current lab')

    def node(self, *args, timeout=30, input_text=None, announce=False):
        if self.worker is None:
            raise GVisorError('unowned_worker', 'node operations require verified ownership')
        return self.lab.runner.run([self.lab.tools['docker'], 'exec', '-i', self.worker, *args],
                                   timeout=timeout, input_text=input_text, announce=announce)

    def node_hash(self, path):
        return self.node('sha256sum', path).split()[0]

    def copy(self, source, target):
        self.lab.runner.run([self.lab.tools['docker'], 'cp', str(source), self.worker + ':' + target], timeout=120)

    def definition(self):
        return {name: digest(self.lab.config / name) for name in
                ('gvisor.lock.json', 'gvisor-runsc.toml', 'gvisor-runtimeclass.json')}

    def runtime_class(self, name=RUNTIME, handler=RUNTIME):
        obj = json.loads((self.lab.config / 'gvisor-runtimeclass.json').read_text()
                         .replace('__SESSION__', self.state['session']))
        obj['metadata']['name'], obj['handler'] = name, handler
        return obj

    def ensure_runtime_class(self, obj, create=True):
        name = obj['metadata']['name']
        found = self.lab.kubectl('get', 'runtimeclass', name, '--ignore-not-found', '-o', 'json')
        if found.strip():
            actual = json.loads(found)
            if (actual['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
                    actual.get('handler') != obj['handler'] or actual.get('scheduling') != obj['scheduling']):
                raise GVisorError('unowned_runtimeclass', 'runtime class ownership or definition changed')
            return actual
        if not create:
            raise GVisorError('runtimeclass_missing', 'installed runtime class was removed')
        return json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj)))

    def install(self, archive):
        files = checked_archive(archive, self.lock)  # Verify before executing binaries or changing the node.
        with self.lab.operation_lock():
            self.attach()
            previous = self.state.get('gvisor')
            actual = self.node_hash(CONFIG)
            if previous:
                if previous['definition'] != self.definition() or previous['worker_id'] != self.worker:
                    raise GVisorError('gvisor_definition_changed', 'recreate the lab before changing runtime version/config')
                if actual not in (previous['original_config_sha256'], previous['installed_config_sha256']):
                    raise GVisorError('containerd_config_changed', 'refusing to overwrite an external config change')
                configured = (self.lab.state_dir / 'gvisor-containerd.toml').read_text()
                if hashlib.sha256(configured.encode()).hexdigest() != previous['installed_config_sha256']:
                    raise GVisorError('gvisor_state_changed', 'local installation config was modified')
            else:
                # Refuse collisions before installing anything, including a pre-existing runtime class.
                found = self.lab.kubectl('get', 'runtimeclass', RUNTIME, '--ignore-not-found', '-o', 'json')
                if found.strip():
                    raise GVisorError('unowned_runtimeclass', 'runtime class already exists without installation ownership')
                self.node('sh', '-c', 'test ! -e /opt/agentbox-gvisor && test ! -L /opt/agentbox-gvisor && '
                          'test ! -e /usr/local/bin/runsc && test ! -L /usr/local/bin/runsc && '
                          'test ! -e /etc/containerd/agentbox-runsc.toml')
                original = self.node('cat', CONFIG)
                configured = runtime_config(original)
                if self.node('systemctl', 'show', 'kubelet.slice', '-p', 'ControlGroup', '--value').strip() != '/kubelet.slice':
                    raise GVisorError('unsupported_workload_slice', 'expected the pinned kind kubelet.slice')
                original_swap = self.node('systemctl', 'show', 'kubelet.slice', '-p', 'MemorySwapMax', '--value').strip()
                (self.lab.state_dir / 'gvisor-containerd-original.toml').write_text(original)
                (self.lab.state_dir / 'gvisor-containerd.toml').write_text(configured)
                previous = {'definition': self.definition(), 'worker_id': self.worker,
                            'original_config_sha256': actual,
                            'installed_config_sha256': hashlib.sha256(configured.encode()).hexdigest(),
                            'files_sha256': files, 'original_workload_swap_max': original_swap}
                self.state['gvisor'] = previous
            if previous.get('phase') == 'ready':
                self.verify_installation()
                return {'already_installed': True, 'release': self.lock['release'], 'worker_id': self.worker,
                        'definition': self.definition()}
            previous['phase'] = 'installing'
            self.lab.write_state(self.state)
            try:
                stage = '/var/tmp/agentbox-gvisor-' + self.state['session']
                self.node('mkdir', '-p', stage)
                # kind's minimal node image has tar but no bzip2. Decompress the
                # verified archive locally with stdlib; do not apt-install tools.
                with tempfile.TemporaryDirectory(prefix='gvisor-', dir=self.lab.state_dir) as temporary:
                    tar_path = Path(temporary) / 'gvisor.tar'
                    with bz2.open(archive, 'rb') as source, tar_path.open('wb') as target:
                        shutil.copyfileobj(source, target)
                    tar_sha256 = digest(tar_path)
                    self.copy(tar_path, stage + '/gvisor.tar')
                self.copy(self.lab.state_dir / 'gvisor-containerd.toml', stage + '/config.toml')
                self.copy(self.lab.config / 'gvisor-runsc.toml', stage + '/runsc.toml')
                self.node('bash', '-s', '--', stage, tar_sha256, actual,
                          previous['installed_config_sha256'], timeout=180, input_text='''set -euo pipefail
stage=$1
expected_archive=$2
expected_current=$3
expected_config=$4
test "$(sha256sum "$stage/gvisor.tar" | cut -d ' ' -f 1)" = "$expected_archive"
test "$(sha256sum /etc/containerd/config.toml | cut -d ' ' -f 1)" = "$expected_current"
test "$(sha256sum "$stage/config.toml" | cut -d ' ' -f 1)" = "$expected_config"
mkdir -p /opt/agentbox-gvisor
tar -xf "$stage/gvisor.tar" --no-same-owner -C /opt/agentbox-gvisor
if [ -L /usr/local/bin/runsc ]; then
  test "$(readlink /usr/local/bin/runsc)" = /opt/agentbox-gvisor/runsc
else
  test ! -e /usr/local/bin/runsc
  ln -s /opt/agentbox-gvisor/runsc /usr/local/bin/runsc
fi
install -m 0644 "$stage/runsc.toml" /etc/containerd/agentbox-runsc.toml
containerd --config "$stage/config.toml" config dump >/dev/null
install -m 0644 "$stage/config.toml" /etc/containerd/config.agentbox-next.toml
mv /etc/containerd/config.agentbox-next.toml /etc/containerd/config.toml
systemctl set-property kubelet.slice MemorySwapMax=0
systemctl restart containerd
''')
                for name, expected in files.items():
                    if self.node_hash(INSTALL + '/' + name) != expected:
                        raise GVisorError('installed_binary_changed', name)
                version = self.node(INSTALL + '/runsc', '--version')
                if 'release-' + self.lock['release'] not in version:
                    raise GVisorError('gvisor_version_mismatch', version)
                runtime = self.registration()
                rc = self.ensure_runtime_class(self.runtime_class())
                self.lab.kubectl('wait', '--for=condition=Ready', 'node', self.worker_name, '--timeout=90s', timeout=110)
                previous.update(phase='ready', runtimeclass_uid=rc['metadata']['uid'])
                result = {'release': self.lock['release'], 'platform': 'systrap', 'worker_id': self.worker,
                          'files_sha256': files, 'runtime': runtime, 'version': version,
                          'runtimeclass_uid': rc['metadata']['uid'], 'definition': self.definition(),
                          'workload_swap_policy': {'unit': 'kubelet.slice', 'MemorySwapMax': 0,
                                                   'original': previous['original_workload_swap_max']}}
                self.save('gvisor-install.json', result)
                return result
            except BaseException:
                previous['phase'] = 'failed'
                raise
            finally:
                self.lab.write_state(self.state)

    def registration(self):
        info = json.loads(self.node('crictl', 'info'))['config']['containerd']
        runtime = info['runtimes'].get(RUNTIME, {})
        if (runtime.get('runtimeType') != 'io.containerd.runsc.v1' or
                runtime.get('runtimePath') != INSTALL + '/containerd-shim-runsc-v1' or
                runtime.get('options', {}).get('ConfigPath') != RUNSC_CONFIG or
                info.get('defaultRuntimeName') != 'runc'):
            raise GVisorError('runtime_not_registered', 'CRI runtime configuration differs from the lab profile')
        if (self.node('systemctl', 'show', 'kubelet.slice', '-p', 'MemorySwapMax', '--value').strip() != '0' or
                self.node('cat', '/sys/fs/cgroup/kubelet.slice/memory.swap.max').strip() != '0'):
            raise GVisorError('swap_limit_missing', 'owned worker workload slice must disable swap')
        return runtime

    def verify_installation(self):
        installed = self.state.get('gvisor', {})
        if (installed.get('phase') != 'ready' or installed.get('definition') != self.definition() or
                installed.get('worker_id') != self.worker):
            raise GVisorError('gvisor_not_ready', 'install the pinned gVisor profile first')
        if self.node_hash(CONFIG) != installed['installed_config_sha256']:
            raise GVisorError('containerd_config_changed', 'installed config no longer matches')
        if self.node_hash(RUNSC_CONFIG) != digest(self.lab.config / 'gvisor-runsc.toml'):
            raise GVisorError('gvisor_config_changed', 'runsc configuration changed')
        for name, expected in installed['files_sha256'].items():
            if self.node_hash(INSTALL + '/' + name) != expected:
                raise GVisorError('installed_binary_changed', name)
        self.registration()
        rc = self.ensure_runtime_class(self.runtime_class(), create=False)
        if rc['metadata']['uid'] != installed['runtimeclass_uid']:
            raise GVisorError('runtimeclass_identity_changed', 'runtime class was replaced')

    def job(self, mode, image, runtime=RUNTIME):
        name = 'agentbox-gvisor-' + mode + '-' + uuid.uuid4().hex[:8]
        obj = json.loads((self.lab.config / 'gvisor-job.json').read_text()
                         .replace('__NAME__', name).replace('__SESSION__', self.state['session'])
                         .replace('__IMAGE__', image))
        pod = obj['spec']['template']['spec']
        pod['runtimeClassName'] = runtime
        source = (self.lab.root / 'scripts/lib/gvisor_guest_probe.py').read_text()
        pod['containers'][0]['command'] = ['/usr/local/bin/python3', '-I', '-B', '-u', '-c', source,
                                          'success' if mode == 'missing' else mode]
        if mode == 'deadline':
            obj['spec']['activeDeadlineSeconds'] = 45
        self.save(name + '-manifest.json', obj)
        created = json.loads(self.lab.kubectl('-n', 'agentbox-platform', 'create', '-f', '-', '-o', 'json',
                                            input_text=json.dumps(obj)))
        return {'name': name, 'uid': created['metadata']['uid'], 'mode': mode,
                'runtime_class': runtime, 'image': image, 'cleanup_complete': False}

    def pods(self, record):
        value = json.loads(self.lab.kubectl('-n', 'agentbox-platform', 'get', 'pods', '-l',
                                           'job-name=' + record['name'], '-o', 'json', announce=False))['items']
        for pod in value:
            containers = pod['spec'].get('containers', [])
            if (pod['metadata'].get('labels', {}).get(OWNER) != self.state['session'] or
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

    def processes(self, sandbox):
        text = self.node('sh', '-s', '--', sandbox,
                         input_text=(self.lab.root / 'scripts/lib/gvisor_node_processes.sh').read_text())
        result = []
        for line in text.splitlines():
            pid, ticks, group, exe, args = line.split('\t', 4)
            if not pid.isdigit() or not ticks.isdigit() or '..' in PurePosixPath(group).parts:
                raise GVisorError('invalid_process_evidence', line)
            result.append({'pid': int(pid), 'start_ticks': ticks, 'cgroup': group, 'exe': exe, 'args': args})
        return result

    def runtime_evidence(self, pod, memory_bytes=256 * 1024 * 1024, cpu_milli=500):
        if pod['spec'].get('nodeName') != self.worker_name:
            raise GVisorError('wrong_worker', 'probe did not run on the dedicated worker')
        uid = pod['metadata']['uid']
        values = json.loads(self.node('crictl', 'pods', '-o', 'json'))['items']
        matched = [s for s in values if s.get('labels', {}).get('io.kubernetes.pod.uid') == uid]
        if len(matched) != 1 or matched[0].get('runtimeHandler') != RUNTIME:
            raise GVisorError('runtime_evidence_missing', 'CRI sandbox did not select the gVisor handler')
        sandbox = matched[0]['id']
        if not re.fullmatch(r'[a-f0-9]{64}', sandbox):
            raise GVisorError('invalid_runtime_identity', sandbox)
        inspect = json.loads(self.node('crictl', 'inspectp', sandbox))
        state = json.loads(self.node(INSTALL + '/runsc', '--root=' + ROOT, 'state', sandbox))
        processes = self.processes(sandbox)
        owned = [p for p in processes if uid.replace('-', '_') in p['cgroup'] or uid in p['cgroup']]
        if (state.get('status') != 'running' or state.get('id') != sandbox or
                not any(p['pid'] == state.get('pid') for p in owned)):
            raise GVisorError('runtime_evidence_missing', 'no running gVisor process in the Pod resource domain')
        resources = []
        for group in sorted({p['cgroup'] for p in owned}):
            current = PurePosixPath(group)
            # Read limits on the process cgroup and each ancestor inside this node.
            while str(current) != '/':
                base = '/sys/fs/cgroup' + str(current)
                output = self.node('sh', '-c', 'for f in cpu.max memory.max memory.swap.max memory.events; do '
                                   'if [ -f "$1/$f" ]; then printf "%s\\n" "$f"; cat "$1/$f"; fi; done',
                                   'probe', base)
                resources.append({'cgroup': str(current), 'limits': output})
                current = current.parent
        pod_limits = [r['limits'] for r in resources
                      if uid.replace('-', '_') in r['cgroup'] or uid in r['cgroup']]
        if not any(f'memory.max\n{memory_bytes}\n' in value and
                   any(int(q) * 1000 == cpu_milli * int(period) for q, period in
                       re.findall(r'cpu.max\n(\d+) (\d+)\n', value)) for value in pod_limits):
            raise GVisorError('resource_limits_mismatch', f'Pod ancestor must enforce {cpu_milli}m CPU and {memory_bytes} memory bytes')
        if not any('memory.swap.max\n0\n' in r['limits'] for r in resources):
            raise GVisorError('swap_limit_missing', 'gVisor process must have swap disabled by an ancestor')
        outside = [p for p in processes if p not in owned]
        self.save(pod['metadata']['name'] + '-runtime.json', {'cri': inspect, 'runsc': state,
                                                            'processes': processes, 'resources': resources})
        return {'sandbox_id': sandbox, 'processes': processes, 'resources': resources,
                'runsc_state': state, 'runtime_handler': RUNTIME, 'outside_pod_processes': outside}

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
                                      ready=ready, runtime=self.runtime_evidence(pod))
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
            live = self.processes(sandbox) if sandbox else []
            cri = json.loads(self.node('crictl', 'pods', '-o', 'json'))['items']
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
        return self.node('sh', '-c', 'for f in memory.max memory.current memory.peak memory.swap.max '
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
        rc = self.ensure_runtime_class(self.runtime_class(name, name))
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
            self.attach()
            self.verify_installation()
            self.lab.namespace(self.state)
            try:
                for mode in (mode for mode in cases if mode != 'missing'):
                    print('[platform] gVisor lifecycle probe: ' + mode, flush=True)
                    record = self.job(mode, image)
                    results.append(record)
                    pod = self.wait_ready(record, timeout)
                    self.save('gvisor-test.json', results)
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
                self.save('gvisor-test.json', results)
