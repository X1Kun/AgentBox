import json
import time
import subprocess
import urllib.parse
from lib.core.errors import BatchError
from lib.core.validation import require
from lib.manifests import read_yaml
from lib.observability.stack import Observability

def instrument(lab, obj):
    patches = read_yaml(lab.config / 'agentbox/overlays/lab/api-metrics-patch.yaml', mapping=False)
    container = obj['spec']['template']['spec']['containers'][0]
    container['args'].append(patches[0]['value'])
    container['ports'].append(patches[1]['value'])
    return obj


class Forward:
    def __init__(self, lab, namespace, pod, path):
        self.path = path
        self.log = path.open('w')
        self.process = subprocess.Popen([lab.tools['kubectl'], '--kubeconfig', str(lab.kubeconfig),
            '--context', 'kind-agentbox-platform-lab', '-n', namespace, 'port-forward', '--address=127.0.0.1',
            'pod/' + pod, ':9091'], stdin=subprocess.DEVNULL, stdout=self.log, stderr=self.log)
        self.address = None
        until = time.monotonic() + 20
        while time.monotonic() < until:
            require(self.process.poll() is None, 'metrics_forward_failed', 'metrics forward exited')
            for line in path.read_text().splitlines():
                if line.startswith('Forwarding from 127.0.0.1:'):
                    self.address = line.split(' ')[2]
                    return
            time.sleep(.1)
        self.close()
        raise BatchError('metrics_forward_timeout', 'metrics forward was not ready')

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill(); self.process.wait(timeout=5)
        self.log.close()


class ObservationEnvironment:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.stack = Observability(lab)
        self.namespaces = {}
        self.forwards = {}
        self.scope = {}
        self.api_uids = {}
        self.record = {'schema_version': 1, 'checks': [], 'cleanup_complete': False, 'all_passed': False}


    def save(self):
        self.lab.write_json(self.output / 'observability-test.json', self.record)


    def mark(self, name):
        self.record['checks'].append({'name': name, 'passed': True}); self.save()


    def query(self, expr):
        data = json.loads(self.stack.http(19090, '/api/v1/query?' + urllib.parse.urlencode({'query': expr})))
        require(data['status'] == 'success', 'prometheus_query_failed', 'query failed')
        return data['data']['result']


    def wait(self, expr, predicate=lambda values: bool(values), timeout=45):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            result = self.query(expr)
            if predicate(result):
                self.record.setdefault('queries', {})[expr] = result; self.save(); return result
            time.sleep(.5)
        raise BatchError('observation_not_converged', expr)


    def attach(self, domain, namespace, pod, api_namespace=None):
        if self.api_uids.get(domain) != pod['uid']:
            if domain in self.forwards:
                self.forwards.pop(domain).close()
            self.forwards[domain] = Forward(self.lab, api_namespace or namespace, pod['name'], self.output / (domain + '-metrics-forward.log'))
            self.api_uids[domain] = pod['uid']
            self.stack.targets({key: value.address for key, value in self.forwards.items()})
        if self.scope.get(domain) != namespace:
            self.scope[domain] = namespace
            self.stack.watch(self.scope)
        self.wait('up{job="agentbox-api",instance="' + domain + '"} == 1')
        self.wait('agentbox_inventory_valid{domain="' + domain + '"} == 1')


