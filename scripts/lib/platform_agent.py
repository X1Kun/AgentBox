"""Deterministic Agent -> stdio MCP -> API -> real gVisor acceptance."""
import base64
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlsplit
import uuid

from lib.platform_api_execution import APIExecutionAcceptance
from lib.platform_sessions import Sessions, SessionError

TOOLS = {'session_get_v2', 'session_execute_v2', 'session_write_file_v2', 'session_read_file_v2'}
MODEL_KEY = 'deterministic-model-fixture-key'


def legacy_inventory():
    result = subprocess.run(['ps', '-eo', 'pid=,comm='], capture_output=True, text=True, check=True, timeout=10)
    processes = []
    for line in result.stdout.splitlines():
        fields = line.split(None, 1)
        if len(fields) == 2 and fields[1] in ('agentboxd', 'agentbox-gateway', 'agentbox-gatewa'):
            processes.append({'pid': int(fields[0]), 'name': fields[1]})
    units = {}
    for name in ('agentbox.service', 'agentbox-gateway.service'):
        value = subprocess.run(['systemctl', 'show', name, '--property=LoadState,ActiveState,ExecMainPID,FragmentPath'],
                               capture_output=True, text=True, timeout=10)
        units[name] = dict(line.split('=', 1) for line in value.stdout.splitlines() if '=' in line)
    return {'processes': processes, 'units': units}


class FixtureModel:
    def __init__(self, acceptance, scenario, runtime, work, token):
        self.acceptance = acceptance
        self.scenario = scenario
        self.runtime, self.work, self.token = runtime, work, token
        self.calls = 0
        self.binding = None
        self.error = None
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                try:
                    raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                    acceptance.require(self.path == '/chat/completions', 'wrong deterministic provider path')
                    acceptance.require(self.headers.get('Authorization') == 'Bearer ' + MODEL_KEY, 'model credential missing')
                    acceptance.require(token.encode() not in raw, 'API credential reached model')
                    request = json.loads(raw)
                    acceptance.require({t['function']['name'] for t in request['tools']} == TOOLS, 'model tool set includes lifecycle or legacy tools')
                    fixture.calls += 1
                    messages = request['messages']
                    binding = json.loads(messages[0]['content'].split('\nBound session: ', 1)[1])
                    if fixture.binding is None:
                        fixture.binding = binding
                        acceptance.capture_runtime(binding, runtime, work)
                    acceptance.require(binding == fixture.binding, 'Agent rebound its session')
                    message = fixture.reply(messages, binding)
                    finish = 'tool_calls' if message.get('tool_calls') else 'stop'
                    body = json.dumps({'choices': [{'message': message, 'finish_reason': finish}]}).encode()
                    self.send_response(200); self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
                except Exception as error:
                    fixture.error = type(error).__name__
                    self.send_response(500); self.end_headers()

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join(timeout=5)

    @property
    def base(self):
        return 'http://127.0.0.1:' + str(self.server.server_port)

    def call(self, name, binding, **kwargs):
        args = dict(binding)
        args.update(kwargs)
        if name == 'session_get_v2':
            args.pop('identity_revision', None)
        return {'id': self.scenario + '-' + str(self.calls), 'type': 'function',
                'function': {'name': name, 'arguments': json.dumps(args)}}

    def reply(self, messages, binding):
        a = self.acceptance
        if self.scenario != 'flow':
            a.require(self.calls == 1, 'uncertain operation returned to model or was replayed')
            code = "print('once')" if self.scenario == 'lost-reply' else "import time\nprint('started',flush=True)\ntime.sleep(30)"
            first = self.call('session_execute_v2', binding, code=code, timeout_seconds=1 if self.scenario == 'timeout' else 10)
            second = self.call('session_execute_v2', binding, code="print('must not run')")
            second['id'] += '-forbidden'
            return {'role': 'assistant', 'tool_calls': [first, second]}
        last = json.loads(messages[-1]['content'])['response'] if messages[-1]['role'] == 'tool' else None
        if self.calls == 1:
            write = self.call('session_write_file_v2', binding, path='input.csv', content_base64=base64.b64encode(b'value\n2\n3\n5\n').decode())
            bad = self.call('session_execute_v2', binding, code="raise ValueError('expected repair')")
            bad['id'] += '-program-error'
            return {'role': 'assistant', 'tool_calls': [write, bad]}
        if self.calls == 2:
            a.require(last['exit_code'] != 0 and 'ValueError' in last['stderr'], 'program error not returned for repair')
            code = "import csv,json\nrows=list(csv.DictReader(open('input.csv')))\ntotal=sum(int(r['value']) for r in rows)\njson.dump({'total':total,'count':len(rows)},open('report.json','w'))\nprint(total)"
            return {'role': 'assistant', 'tool_calls': [self.call('session_execute_v2', binding, code=code)]}
        if self.calls == 3:
            a.require(last['exit_code'] == 0 and last['stdout'].strip() == '10', 'analysis result wrong')
            return {'role': 'assistant', 'tool_calls': [self.call('session_read_file_v2', binding, path='report.json')]}
        if self.calls == 4:
            a.require(json.loads(base64.b64decode(last['content_base64'])) == {'count': 3, 'total': 10}, 'downloaded report wrong')
            return {'role': 'assistant', 'tool_calls': [self.call('session_execute_v2', binding,
                code="import json\nr=json.load(open('report.json'))\nprint(r['total']+1)\nprint('total' in globals())")]}
        a.require(self.calls == 5 and last['stdout'].splitlines() == ['11', 'False'], 'incremental file call or interpreter isolation failed')
        return {'role': 'assistant', 'content': 'Observed total=10, count=3, next=11, fresh_globals=False.'}


class LostReplyProxy:
    """Fixture only: forward one real successful execution, then drop its reply."""
    def __init__(self, base, tls_context):
        target = urlsplit(base)
        proxy = self
        self.executions = 0

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def forward(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                upstream = http.client.HTTPSConnection(target.hostname, target.port, context=tls_context, timeout=80)
                try:
                    upstream.request(self.command, self.path, body if body else None,
                                     {'Authorization': self.headers.get('Authorization', ''), 'Content-Type': 'application/json'})
                    response = upstream.getresponse(); data = response.read(1 << 20)
                    if self.command == 'POST' and self.path.endswith('/execute'):
                        proxy.executions += 1
                        self.connection.shutdown(socket.SHUT_RDWR); self.connection.close(); return
                    self.send_response(response.status); self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
                finally:
                    upstream.close()
            do_GET = do_POST = do_DELETE = forward

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True); self.thread.start()

    @property
    def base(self):
        return 'http://127.0.0.1:' + str(self.server.server_port)

    def close(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join(timeout=5)


class AgentAcceptance(APIExecutionAcceptance):
    def save(self):
        (self.output/'agent-test.json').write_text(json.dumps(self.record, indent=2, sort_keys=True)+'\n')

    def capture_runtime(self, binding, runtime, work):
        sid = binding['session_id']
        claim = json.loads(self.lab.kubectl('-n', work, 'get', 'sandboxclaim', sid.split('.')[0], '-o', 'json', announce=False))
        self.require(sid == claim['metadata']['name'] + '.' + claim['metadata']['uid'], 'wrong Claim incarnation')
        self.require(claim['metadata']['labels'].get('agentbox.dev/api-instance') == self.instance, 'foreign Agent instance')
        refs = json.loads(claim['metadata']['annotations']['agentbox.dev/api-binding'])
        pod = json.loads(self.lab.kubectl('-n', work, 'get', 'pod', refs['pod']['name'], '-o', 'json', announce=False))
        self.require(pod['metadata']['uid'] == refs['pod']['uid'], 'wrong Agent Pod UID')
        self.record['runtime_evidence'][pod['metadata']['uid']] = runtime.runtime_evidence(pod, memory_bytes=512*1024*1024)
        self.record.setdefault('sessions', []).append(dict(binding))
        self.save()

    def test(self):
        with self.lab.operation_lock():
            sessions = Sessions(self.lab); runtime = sessions.attach(); bundle = sessions.bundle(); sessions.verify(bundle)
            inventory = legacy_inventory()
            self.require(not inventory['processes'], 'legacy AgentBox processes must be retired before acceptance')
            self.require(all(v.get('ActiveState') in ('inactive', 'failed') and v.get('ExecMainPID') == '0' for v in inventory['units'].values()), 'legacy service is active or cannot be inspected')
            run = uuid.uuid4().hex[:10]; namespaces = {}
            self.instance = 'api-' + run
            self.record.update(run=run, owner=sessions.state['session'], namespaces=namespaces,
                               runtime_evidence={}, legacy_inventory=inventory, agents={})
            directory = self.lab.state_dir/'agent-build'; directory.mkdir(exist_ok=True)
            for name in ('agentbox-agent', 'agentbox-mcp'):
                binary = directory/name
                self.lab.runner.run(['env', 'CGO_ENABLED=0', 'go', 'build', '-p=2', '-mod=readonly', '-trimpath', '-buildvcs=false', '-o', binary, './cmd/'+name], timeout=900)
                self.record.setdefault('binaries', {})[name] = hashlib.sha256(binary.read_bytes()).hexdigest()
            try:
                image = self.build_image(sessions)
                for role in ('work', 'client'):
                    namespaces[role] = sessions.namespace('agentbox-agent-'+role+'-'+run, 'agent-'+role, run)
                work, client = namespaces['work']['name'], namespaces['client']['name']
                params = {'OWNER': sessions.state['session'], 'RUN': run, 'WORK_NS': work, 'CLIENT_NS': client,
                          'RUNTIME_IMAGE': bundle['images']['runtime']['name'], 'API_IMAGE': image}
                for name in ('session-template.json', 'api-rbac.json', 'api-quota.json'):
                    sessions.create(sessions.render(name, params))
                sessions.create(sessions.client_policy(client, work, run))
                until = time.monotonic()+120
                while time.monotonic() < until:
                    q = json.loads(self.lab.kubectl('-n',work,'get','resourcequota','agentbox-api-budget','-o','json',announce=False))
                    if q.get('status',{}).get('hard') == q['spec']['hard']: break
                    time.sleep(.5)
                else: raise SessionError('agent_quota_not_ready', 'quota was not observed')
                alice, bob = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
                with tempfile.TemporaryDirectory(prefix='agent-private-', dir=self.lab.state_dir) as tmp:
                    tmp = Path(tmp)
                    self.scoped_credentials(tmp, work, client)
                    self.secret(tmp, client, alice, bob)
                    sessions.create(sessions.render('api-deployment.json', params))
                    self.connect(client)
                    for scenario in ('flow', 'timeout', 'lost-reply'):
                        model = FixtureModel(self, scenario, runtime, work, alice)
                        proxy = LostReplyProxy(self.base, self.tls_context) if scenario == 'lost-reply' else None
                        try:
                            args = [str(directory/'agentbox-agent'), '--api-url', proxy.base if proxy else self.base,
                                    '--api-ca-file', str(tmp/'tls.crt'), '--mcp-command', str(directory/'agentbox-mcp'),
                                    '--deepseek-base-url', model.base, '--model', 'deterministic-fixture', '--prompt', 'Use the bound session to compute and export the requested result.',
                                    '--json', '--ready-timeout=180s', '--cleanup-timeout=90s']
                            env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'LANG': 'C.UTF-8',
                                   'AGENTBOX_API_TOKEN': alice, 'DEEPSEEK_API_KEY': MODEL_KEY}
                            completed = subprocess.run(args, env=env, capture_output=True, text=True, timeout=300)
                            result = json.loads(completed.stdout)
                            (self.output/('agent-'+scenario+'.json')).write_text(json.dumps(result,indent=2,sort_keys=True)+'\n')
                            self.record['agents'][scenario] = {'exit_code':completed.returncode, 'model_calls':model.calls, 'result':result}
                            self.save()
                            self.require(model.error is None, 'deterministic model assertions failed: '+str(model.error))
                            self.require(result['cleanup_complete'], 'Agent did not confirm API object cleanup')
                            if scenario == 'flow':
                                self.require(completed.returncode == 0 and result['tool_calls'] == 5 and not result['outcome_unknown'], 'Agent flow did not succeed')
                                self.require(json.loads(base64.b64decode(result['artifacts'][0]['content_base64'])) == {'total':10,'count':3}, 'Agent artifact not exported')
                            else:
                                self.require(completed.returncode != 0 and result['tool_calls'] == 1 and result['outcome_unknown'] and model.calls == 1, 'uncertain call replayed or continued')
                            if proxy: self.require(proxy.executions == 1, 'lost execution response was replayed')
                            self.mark('agent_'+scenario.replace('-','_')+'_and_confirmed_closure')
                        finally:
                            if proxy: proxy.close()
                            model.close()
                    logs = self.lab.kubectl('-n', client, 'logs', self.record['api_pod']['name'], '-c', 'api', announce=False)
                    events = [json.loads(line) for line in logs.splitlines() if line.startswith('{')]
                    for scenario, count in [('flow',3),('timeout',1),('lost-reply',1)]:
                        sid = self.record['agents'][scenario]['result']['session_id']
                        self.require(sum(e.get('phase')=='execute' and e.get('session_id')==sid for e in events)==count, 'API dispatch count differs from Agent calls')
                    self.record['api_execution_counts'] = {'flow':3,'timeout':1,'lost-reply':1}
                    self.mark('native_api_execution_counts_confirm_no_replay')
                    self.stop()
                until = time.monotonic()+60
                while time.monotonic() < until:
                    processes = [p for v in self.record['runtime_evidence'].values() for p in runtime.processes(v['sandbox_id'])]
                    cri = [v for v in json.loads(runtime.node('crictl','pods','-o','json'))['items'] if v.get('metadata',{}).get('namespace')==work]
                    if not processes and not cri: break
                    time.sleep(.5)
                else: raise SessionError('agent_runtime_cleanup_incomplete','runtime remained after Agent closure')
                self.record['runtime_cleanup'] = {'remaining_processes':[],'remaining_cri_sandboxes':[]}
                sessions.close_namespaces(namespaces, run)
                self.record.update(cleanup_complete=True, all_passed=True, development_cluster_retained=True)
                return self.record
            finally:
                self.stop(); self.save()
