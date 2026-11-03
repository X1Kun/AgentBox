"""Owned, loopback-only observability services and read-only watch credentials."""
import base64
import hashlib
import ipaddress
import json
import os
import re
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request

import yaml

from lib.platform_batch import BatchError, require
from lib.platform_manifests import read_yaml

OWNER = 'agentbox.dev/session'
PROJECT = 'agentbox-observability'


class Observability:
    def __init__(self, lab):
        self.lab = lab
        self.source = lab.config / 'components/observability'
        self.directory = lab.root / '.local/observability'
        self.config = self.directory / 'config'
        self.state_path = self.directory / 'state.json'
        self.lock = read_yaml(self.source / 'versions.lock.yaml')

    def state(self):
        state = json.loads(self.state_path.read_text())
        owned, _ = self.lab.owned(complete=True)
        require(state['owner'] == owned['session'] and state['docker_id'] == owned['docker_id'],
                'unowned_observability', 'observation state belongs to another lab session')
        return state

    def _compose(self, *args):
        return [self.lab.tools['docker'], 'compose', '--project-name', PROJECT, '--env-file',
                self.directory / 'private.env', '-f', self.source / 'compose.yaml', *args]

    def _owned_containers(self, owner):
        ids = self.lab.runner.run([self.lab.tools['docker'], 'ps', '-aq', '--filter',
            'label=com.docker.compose.project=' + PROJECT], announce=False).split()
        for value in ids:
            labels = json.loads(self.lab.runner.run([self.lab.tools['docker'], 'inspect', value,
                '--format', '{{json .Config.Labels}}'], announce=False))
            require(labels.get(OWNER) == owner, 'unowned_observability_container', 'refusing to manage another project/session container')
        volumes = self.lab.runner.run([self.lab.tools['docker'], 'volume', 'ls', '-q', '--filter',
            'label=com.docker.compose.project=' + PROJECT], announce=False).split()
        for value in volumes:
            labels = json.loads(self.lab.runner.run([self.lab.tools['docker'], 'volume', 'inspect', value,
                '--format', '{{json .Labels}}'], announce=False))
            require(labels.get(OWNER) == owner, 'unowned_observability_volume', 'refusing to use another session volume')
        return ids

    def render(self, owner):
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        require(not self.directory.is_symlink(), 'invalid_observability_path', 'symlinked observation state is forbidden')
        for path in ('prometheus', 'grafana/provisioning/datasources', 'grafana/provisioning/dashboards', 'grafana/dashboards'):
            (self.config / path).mkdir(parents=True, exist_ok=True)
        for src, dst in [('prometheus.yaml', 'prometheus/prometheus.yaml'), ('rules.yaml', 'prometheus/rules.yaml'),
                ('datasource.yaml', 'grafana/provisioning/datasources/agentbox.yaml'),
                ('provider.yaml', 'grafana/provisioning/dashboards/agentbox.yaml')]:
            shutil.copyfile(self.source / src, self.config / dst)
        dashboard = read_yaml(self.source / 'dashboard.yaml')
        (self.config / 'grafana/dashboards/agentbox.json').write_text(json.dumps(dashboard, indent=2) + '\n')
        targets = self.config / 'prometheus/targets.yaml'
        if not targets.exists():
            targets.write_text('[]\n')
        password = self.directory / 'grafana-password'
        if not password.exists():
            password.write_text(secrets.token_urlsafe(32) + '\n')
            password.chmod(0o600)
        values = {'AGENTBOX_OBS_OWNER': owner, 'AGENTBOX_OBS_CONFIG': str(self.config),
                  'AGENTBOX_OBS_GRAFANA_PASSWORD': password.read_text().strip()}
        env = self.directory / 'private.env'
        env.write_text(''.join(k + '=' + json.dumps(v) + '\n' for k, v in values.items()))
        env.chmod(0o600)

    def http(self, port, path, credentials=False):
        request = urllib.request.Request('http://127.0.0.1:' + str(port) + path)
        if credentials:
            value = 'admin:' + (self.directory / 'grafana-password').read_text().strip()
            request.add_header('Authorization', 'Basic ' + base64.b64encode(value.encode()).decode())
        # Ignore ambient proxy settings for this explicitly local control plane.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=5) as response:
            data = response.read((8 << 20) + 1)
            require(len(data) <= 8 << 20, 'observability_response_too_large', 'bounded query response exceeded')
            return data

    def up(self):
        with self.lab.operation_lock():
            owned, _ = self.lab.owned(complete=True)
            previous = self.state() if self.state_path.exists() else {}
            existing = self._owned_containers(owned['session'])
            if not existing:
                for port in (19090, 13000, 19100):
                    with socket.socket() as sock:
                        sock.bind(('127.0.0.1', port))
            self.render(owned['session'])
            state = {'schema_version': 1, 'owner': owned['session'], 'docker_id': owned['docker_id'],
                     'definition': {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in self.source.glob('*.yaml')}}
            for key in ('observer', 'rbac'):
                if key in previous:
                    state[key] = previous[key]
            self.lab.write_json(self.state_path, state)
            for role in self.lock['images'].values():
                info = json.loads(self.lab.runner.run([self.lab.tools['docker'], 'image', 'inspect', role['image']], announce=False))[0]
                require(role['image'] in info.get('RepoDigests', []), 'observability_image_changed', 'preload the locked image digest first')
            self.lab.runner.run(self._compose('config', '--quiet'), timeout=30)
            image = self.lock['images']['prometheus']['image']
            self.lab.runner.run([self.lab.tools['docker'], 'run', '--rm', '--network=none',
                '-v', str(self.config / 'prometheus') + ':/etc/prometheus:ro', '--entrypoint=/bin/promtool', image,
                'check', 'config', '/etc/prometheus/prometheus.yaml'], timeout=30)
            self.lab.runner.run(self._compose('up', '-d', '--pull', 'never'), timeout=120)
            until = time.monotonic() + 90
            while time.monotonic() < until:
                try:
                    self.http(19090, '/-/ready')
                    self.http(13000, '/api/health')
                    dashboard = json.loads(self.http(13000, '/api/dashboards/uid/agentbox-observation', True))
                    require(dashboard['dashboard']['uid'] == 'agentbox-observation', 'dashboard_not_provisioned', 'unexpected dashboard')
                    return {'prometheus': 'http://127.0.0.1:19090', 'grafana': 'http://127.0.0.1:13000',
                            'dashboard_uid': 'agentbox-observation', 'password_file': str(self.directory / 'grafana-password')}
                except (OSError, urllib.error.URLError, KeyError):
                    time.sleep(.5)
            raise BatchError('observability_start_timeout', 'local monitoring services did not become ready')

    def targets(self, targets):
        path = self.config / 'prometheus/targets.yaml'
        values = []
        for domain, address in targets.items():
            match = re.fullmatch(r'127\.0\.0\.1:([0-9]{1,5})', address)
            require(domain in ('session', 'batch') and match is not None and 0 < int(match[1]) <= 65535,
                    'invalid_metrics_target', 'only explicit local forwards are supported')
            values.append({'targets': [address], 'labels': {'instance': domain}})
        temporary = path.with_suffix('.tmp')
        temporary.write_text(yaml.safe_dump(values, sort_keys=False))
        temporary.replace(path)

    @staticmethod
    def ticks(pid):
        return (Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()[19]

    def stop_observer(self):
        state = self.state()
        proc = state.get('observer')
        if not proc:
            return
        pid = proc['pid']
        path = Path('/proc') / str(pid)
        if path.exists():
            command = (path / 'cmdline').read_bytes().split(b'\0')
            require(self.ticks(pid) == proc['start_ticks'] and str(self.directory / 'agentbox-observer').encode() in command,
                    'observer_process_changed', 'refusing to signal a replaced process')
            os.kill(pid, signal.SIGTERM)
            until = time.monotonic() + 12
            while path.exists() and time.monotonic() < until:
                try:
                    os.waitpid(pid, os.WNOHANG)
                except ChildProcessError:
                    pass
                time.sleep(.1)
            require(not path.exists(), 'observer_stop_timeout', 'read-only observer did not stop')
        state.pop('observer', None)
        self.lab.write_json(self.state_path, state)

    def _ensure(self, obj, state):
        meta = obj['metadata']
        meta.setdefault('labels', {})[OWNER] = state['owner']
        args = ['-n', meta['namespace']] if meta.get('namespace') else []
        text = self.lab.kubectl(*args, 'get', obj['kind'], meta['name'], '--ignore-not-found', '-o', 'json', announce=False)
        if text.strip():
            current = json.loads(text)
            require(current['metadata'].get('labels', {}).get(OWNER) == state['owner'], 'unowned_observer_rbac', meta['name'])
            previous = next((r for r in state.get('rbac', []) if (r['kind'], r['name'], r.get('namespace')) ==
                            (obj['kind'], meta['name'], meta.get('namespace'))), None)
            if previous:
                require(previous['uid'] == current['metadata']['uid'], 'observer_rbac_replaced', meta['name'])
            if obj['kind'] in ('Namespace', 'ServiceAccount'):
                value = current
            else:
                obj['metadata']['resourceVersion'] = current['metadata']['resourceVersion']
                value = json.loads(self.lab.kubectl('replace', '-f', '-', '-o', 'json', input_text=json.dumps(obj), announce=False))
        else:
            value = json.loads(self.lab.kubectl('create', '-f', '-', '-o', 'json', input_text=json.dumps(obj), announce=False))
        ref = {'kind': obj['kind'], 'name': meta['name'], 'namespace': meta.get('namespace'), 'uid': value['metadata']['uid']}
        state['rbac'] = [r for r in state.get('rbac', []) if (r['kind'], r['name'], r.get('namespace')) !=
                         (ref['kind'], ref['name'], ref.get('namespace'))] + [ref]
        self.lab.write_json(self.state_path, state)

    def watch(self, namespaces):
        with self.lab.operation_lock():
            state = self.state()
            require(0 < len(namespaces) <= 2 and len(set(namespaces.values())) == len(namespaces),
                    'invalid_observer_namespace', 'provide one unique namespace per domain')
            # Validate the entire new scope before changing the previous observer or RBAC.
            for domain, namespace in namespaces.items():
                require(domain in ('session', 'batch') and re.fullmatch(r'agentbox-[a-z0-9-]{1,54}[a-z0-9]', namespace),
                        'invalid_observer_namespace', namespace)
                actual = json.loads(self.lab.kubectl('get', 'namespace', namespace, '-o', 'json', announce=False))
                require(actual['metadata'].get('labels', {}).get(OWNER) == state['owner'], 'unowned_observer_namespace', namespace)
            self.stop_observer()
            state = self.state()
            management = 'agentbox-observability'
            self._ensure({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': management}}, state)
            self._ensure({'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'agentbox-observer', 'namespace': management},
                          'automountServiceAccountToken': False}, state)
            for domain, namespace in namespaces.items():
                role = read_yaml(self.source / 'namespace-role.yaml')
                role['metadata']['namespace'] = namespace
                self._ensure(role, state)
                self._ensure({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
                    'metadata': {'name': 'agentbox-observer', 'namespace': namespace},
                    'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'agentbox-observer'},
                    'subjects': [{'kind': 'ServiceAccount', 'name': 'agentbox-observer', 'namespace': management}]}, state)
            nodes = json.loads(self.lab.kubectl('get', 'nodes', '-o', 'json', announce=False))['items']
            name = 'agentbox-observer-' + state['owner'][:12]
            self._ensure({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRole', 'metadata': {'name': name},
                'rules': [{'apiGroups': [''], 'resources': ['namespaces'], 'resourceNames': list(namespaces.values()), 'verbs': ['get']},
                          {'apiGroups': [''], 'resources': ['nodes/metrics'], 'resourceNames': [n['metadata']['name'] for n in nodes], 'verbs': ['get']}]}, state)
            self._ensure({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRoleBinding', 'metadata': {'name': name},
                'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': name},
                'subjects': [{'kind': 'ServiceAccount', 'name': 'agentbox-observer', 'namespace': management}]}, state)
            prefix = [self.lab.tools['kubectl'], '--kubeconfig', str(self.lab.kubeconfig), '--context', 'kind-agentbox-platform-lab']
            # Never route credential-bearing output through Runner or reports.
            token = subprocess.run(prefix + ['-n', management, 'create', 'token', 'agentbox-observer', '--duration=3600s'],
                capture_output=True, text=True, check=True, timeout=30).stdout.strip()
            cluster = json.loads(subprocess.run(prefix + ['config', 'view', '--raw', '--minify', '-o', 'json'],
                capture_output=True, text=True, check=True, timeout=30).stdout)['clusters'][0]['cluster']
            private = self.directory / 'private'
            private.mkdir(exist_ok=True, mode=0o700)
            kubeconfig = private / 'kubeconfig'
            self.lab.write_json(kubeconfig, {'apiVersion': 'v1', 'kind': 'Config', 'clusters': [{'name': 'lab', 'cluster': cluster}],
                'users': [{'name': 'observer', 'user': {'token': token}}], 'contexts': [{'name': 'observer',
                    'context': {'cluster': 'lab', 'user': 'observer'}}], 'current-context': 'observer'})
            kubeconfig.chmod(0o600)
            config = {'namespaces': namespaces, 'nodes': []}
            for node in nodes:
                node_name = node['metadata']['name']
                require(node_name in ('agentbox-platform-lab-worker', 'agentbox-platform-lab-control-plane'), 'unexpected_observer_node', node_name)
                role = 'worker' if node_name.endswith('-worker') else 'control-plane'
                address = next(a['address'] for a in node['status']['addresses'] if a['type'] == 'InternalIP')
                ipaddress.IPv4Address(address)
                certificate = self.lab.runner.run([self.lab.tools['docker'], 'exec', node_name, 'cat', '/var/lib/kubelet/pki/kubelet.crt'], announce=False)
                ca = private / (role + '.pem'); ca.write_text(certificate)
                config['nodes'].append({'role': role, 'url': 'https://' + address + ':10250/metrics/cadvisor',
                                        'ca_file': str(ca), 'server_name': node_name})
            config_path = private / 'observer.json'; self.lab.write_json(config_path, config); config_path.chmod(0o600)
            binary = self.directory / 'agentbox-observer'
            self.lab.runner.run(['env', 'CGO_ENABLED=0', 'go', 'build', '-p=2', '-mod=readonly', '-trimpath',
                '-buildvcs=false', '-o', binary, './cmd/agentbox-observer'], timeout=900)
            events = self.directory / 'events.jsonl'
            with events.open('ab') as log:
                process = subprocess.Popen([binary, '--kubeconfig', kubeconfig, '--config', config_path],
                    stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
            state['observer'] = {'pid': process.pid, 'start_ticks': self.ticks(process.pid), 'config': str(config_path), 'events': str(events), 'namespaces': namespaces}
            self.lab.write_json(self.state_path, state)
            return {'pid': process.pid, 'domains': sorted(namespaces), 'metrics': 'http://127.0.0.1:19101/metrics'}

    def down(self):
        with self.lab.operation_lock():
            state = self.state(); self.stop_observer(); self._owned_containers(state['owner'])
            self.lab.runner.run(self._compose('down'), timeout=120)
            return {'stopped': True, 'volumes_retained': True}
