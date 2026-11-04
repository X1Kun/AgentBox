"""Bounded, read-only experiment evidence; no workload mutation or native errors."""
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import select
import subprocess
import threading
import time
import urllib.parse

METRICS = ('up', 'agentbox_inventory_valid', 'agentbox_inventory_timestamp_seconds',
    'agentbox_inventory_requests_total', 'agentbox_inventory_request_duration_seconds_bucket',
    'agentbox_runtime_observation_valid', 'agentbox_runtime_observation_timestamp_seconds',
    'agentbox_runtime_polls_total', 'agentbox_runtime_poll_duration_seconds_bucket',
    'agentbox_runtime_resource', 'agentbox_inventory_items', 'agentbox_lifecycle_events_total',
    'agentbox_diagnostics_total', 'agentbox_diagnostic_duration_seconds_bucket',
    'process_cpu_seconds_total', 'process_resident_memory_bytes',
    'node_pressure_io_stalled_seconds_total', 'node_cpu_seconds_total', 'node_memory_MemAvailable_bytes')


class MetricsArchiveError(ValueError):
    reason = 'diagnostic_metrics_incomplete'


def archive_metrics(stack, output, start, end):
    series, failures = {}, []
    deadline = time.monotonic()+10
    for expression in METRICS:
        try:
            if time.monotonic() >= deadline:
                raise TimeoutError('archive time budget exhausted')
            path = '/api/v1/query_range?' + urllib.parse.urlencode({'query': expression, 'start': start, 'end': end, 'step': 5})
            value = json.loads(stack.http(19090, path))
            if value.get('status') != 'success':
                raise ValueError('diagnostic query failed')
            series[expression] = value
        except Exception as error:
            series[expression] = {'status': 'unavailable', 'error_type': type(error).__name__}
            failures.append(expression)
    Path(output).write_text(json.dumps(series, separators=(',', ':'))+'\n')
    if failures:
        raise MetricsArchiveError('partial metrics preserved; inspect archive')
    return series


def archive_observer(source, output, start, end):
    """Freeze this run's lines even on failure. Python 3.10 needs microseconds."""
    source = Path(source)
    if not source.exists():
        return {'status': 'missing', 'lines': 0}
    size = source.stat().st_size
    offset = max(0, size-(64 << 20))
    count, skipped = 0, 0
    with source.open('rb') as src, Path(output).open('wb') as dst:
        src.seek(offset)
        if offset:
            src.readline()
        while src.tell() < size:
            line = src.readline(min((1 << 20)+1, size-src.tell()))
            if len(line) > 1 << 20:
                raise ValueError('observer line exceeds evidence budget')
            try:
                event = json.loads(line)
                stamp = re.sub(r'(\.\d{6})\d+', r'\1', event['time'])
                instant = datetime.fromisoformat(stamp.replace('Z', '+00:00')).timestamp()
                if start <= instant <= end:
                    dst.write(line)
                    count += 1
            except (ValueError, KeyError, TypeError, UnicodeError):
                skipped += 1
    return {'status': 'archived', 'lines': count, 'skipped_non_json': skipped,
            'source_bytes': size, 'source_offset': offset, 'source_truncated': offset > 0,
            'started_at': start, 'ended_at': end}


def bounded_command(args, timeout=4, limit=8 << 20):
    """Limit stdout while reading, and kill only our own diagnostic subprocess."""
    process = subprocess.Popen([str(a) for a in args], stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL)
    data = bytearray()
    deadline = time.monotonic()+timeout
    try:
        while True:
            left = deadline-time.monotonic()
            if left <= 0 or not select.select([process.stdout], [], [], left)[0]:
                raise TimeoutError('diagnostic budget expired')
            chunk = os.read(process.stdout.fileno(), 65536)
            if not chunk:
                break
            if len(data)+len(chunk) > limit:
                raise ValueError('diagnostic output exceeds budget')
            data.extend(chunk)
        process.wait(timeout=max(.01, deadline-time.monotonic()))
        if process.returncode:
            raise OSError('diagnostic command failed')
        return data.decode()
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=2)
        process.stdout.close()


def host_sample():
    result = {'pressure': {}, 'disks': {}, 'processes': []}
    for kind in ('cpu', 'memory', 'io'):
        try:
            result['pressure'][kind] = Path('/proc/pressure', kind).read_text().strip()
        except OSError:
            result['pressure'][kind] = None
    for line in Path('/proc/diskstats').read_text().splitlines():
        fields = line.split()
        name = fields[2]
        if Path('/sys/block', name).exists() and not name.startswith(('loop', 'ram')):
            result['disks'][name] = {'raw_counters': [int(v) for v in fields[3:]]}
    result['memory'] = {line.split(':', 1)[0]: line.split(':', 1)[1].strip()
        for line in Path('/proc/meminfo').read_text().splitlines()
        if line.split(':', 1)[0] in ('MemAvailable', 'Dirty', 'Writeback', 'SwapFree')}
    allowed = {'gopls', 'agentbox-api', 'agentbox-observer', 'etcd', 'kube-apiserver',
               'kubelet', 'containerd', 'runsc', 'prometheus', 'python3'}
    for proc in Path('/proc').iterdir():
        if not proc.name.isdigit():
            continue
        try:
            comm = (proc/'comm').read_text().strip()
            if comm not in allowed:
                continue
            fields = (proc/'stat').read_text().rsplit(')', 1)[1].split()
            value = {'pid': int(proc.name), 'comm': comm, 'start_ticks': fields[19], 'state': fields[0]}
            try:
                value['io'] = {k: int(v) for k, v in (line.split(': ') for line in (proc/'io').read_text().splitlines())}
                value['wait_channel'] = (proc/'wchan').read_text().strip()
            except OSError:
                value['io'] = None
                value['io_status'] = 'unavailable'
            result['processes'].append(value)
            if len(result['processes']) >= 256:
                result['processes_truncated'] = True
                break
        except (OSError, ValueError):
            continue
    return result


class ExperimentDiagnostics:
    def __init__(self, lab, output):
        self.lab, self.output = lab, Path(output)
        self.stop_event, self.cleanup_event = threading.Event(), threading.Event()
        self.scope, self.scope_lock = {}, threading.Lock()
        self.threads = []
        self.failures = []
        self.write_lock = threading.Lock()
        self.prefix = [lab.tools['kubectl'], '--kubeconfig', lab.kubeconfig,
                       '--context', 'kind-agentbox-platform-lab', '--request-timeout=3s']

    def set_scope(self, refs):
        with self.scope_lock:
            self.scope = {r['name']: r['uid'] for r in refs}

    def write(self, name, value):
        envelope = {'at': datetime.now(timezone.utc).isoformat(), 'monotonic_seconds': time.monotonic(), **value}
        with self.write_lock:
            with (self.output/name).open('a') as stream:
                stream.write(json.dumps(envelope, separators=(',', ':'))+'\n')

    def start(self):
        def guarded(task):
            try:
                task()
            except Exception as error:
                self.failures.append({'task': task.__name__, 'error_type': type(error).__name__})
        for task in (self.host_loop, self.control_loop):
            thread = threading.Thread(target=guarded, args=(task,), daemon=True)
            thread.start()
            self.threads.append(thread)

    def host_loop(self):
        while not self.stop_event.is_set():
            try:
                self.write('host-diagnostics.jsonl', {'sample': host_sample()})
            except Exception as error:
                self.write('host-diagnostics.jsonl', {'error_type': type(error).__name__})
            self.stop_event.wait(5)

    def control_loop(self):
        next_metrics, next_cleanup = 0, 0
        while not self.stop_event.is_set():
            now = time.monotonic()
            if now >= next_metrics:
                self.capture_controlplane()
                next_metrics = time.monotonic()+30
            if self.cleanup_event.is_set() and now >= next_cleanup:
                self.capture_cleanup()
                next_cleanup = time.monotonic()+5
            self.stop_event.wait(.5)

    def capture_controlplane(self):
        prefixes = ('apiserver_request_duration_seconds_', 'etcd_request_duration_seconds_',
                    'apiserver_flowcontrol_request_wait_duration_seconds_', 'apiserver_current_inflight_requests',
                    'etcd_disk_wal_fsync_duration_seconds_', 'etcd_disk_backend_commit_duration_seconds_',
                    'process_cpu_seconds_total', 'process_resident_memory_bytes')
        commands = {'apiserver': self.prefix+['get', '--raw', '/metrics'],
            'etcd': [self.lab.tools['docker'], 'exec', 'agentbox-platform-lab-control-plane',
                     'curl', '-fsS', '--max-time', '3', 'http://127.0.0.1:2381/metrics']}
        for component, args in commands.items():
            started = time.monotonic()
            try:
                lines = [line for line in bounded_command(args).splitlines() if line.startswith(prefixes)]
                self.write('controlplane-diagnostics.jsonl', {'component': component,
                    'duration_seconds': time.monotonic()-started, 'metrics': lines,
                    'status': 'sampled' if lines else 'missing'})
            except Exception as error:
                self.write('controlplane-diagnostics.jsonl', {'component': component,
                    'duration_seconds': time.monotonic()-started, 'status': 'unavailable', 'error_type': type(error).__name__})

    def begin_cleanup(self):
        self.cleanup_event.set()

    def capture_cleanup(self):
        with self.scope_lock:
            scope = dict(self.scope)
        if not scope:
            return
        try:
            objects = json.loads(bounded_command(self.prefix+['get', 'namespace', *scope, '--ignore-not-found', '-o', 'json']))
            items = objects.get('items', [objects] if objects.get('kind') == 'Namespace' else [])
            snapshot = {'namespaces': [], 'resources': {}, 'cri': []}
            for obj in items:
                meta = obj['metadata']
                if scope.get(meta['name']) != meta['uid']:
                    snapshot['namespaces'].append({'name': meta['name'], 'status': 'identity_changed'})
                    continue
                snapshot['namespaces'].append({'metadata': meta, 'status': obj.get('status')})
                data = json.loads(bounded_command(self.prefix+['-n', meta['name'], 'get', 'pods,pvc,events', '-o', 'json']))
                snapshot['resources'][meta['name']] = [{'kind': o['kind'], 'metadata': {k: o['metadata'].get(k)
                    for k in ('name', 'uid', 'ownerReferences', 'deletionTimestamp', 'finalizers')},
                    'status': o.get('status'), 'reason': o.get('reason')} for o in data['items']]
            cri = json.loads(bounded_command([self.lab.tools['docker'], 'exec', 'agentbox-platform-lab-worker', 'crictl', 'pods', '-o', 'json']))
            snapshot['cri'] = [{'id': s['id'], 'metadata': s.get('metadata'), 'state': s.get('state'), 'runtimeHandler': s.get('runtimeHandler')}
                               for s in cri['items'] if s.get('metadata', {}).get('namespace') in scope]
            self.write('cleanup-resources.jsonl', snapshot)
        except Exception as error:
            self.write('cleanup-resources.jsonl', {'status': 'unavailable', 'error_type': type(error).__name__})

    def stop(self):
        self.stop_event.set()
        for thread in self.threads:
            thread.join(timeout=20)
        return {'status': 'incomplete' if self.failures or any(t.is_alive() for t in self.threads) else 'stopped',
                'failures': self.failures,
                'host_interval_seconds': 5, 'controlplane_interval_seconds': 30,
                'cleanup_interval_seconds': 5, 'scope': 'read-only diagnostics; missing samples are unknown'}
