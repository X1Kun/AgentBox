"""One bounded lifecycle API behavior test with real namespace-scoped credentials."""
import hashlib
import http.client
import json
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

from lib.platform_sessions import Sessions, SessionError, OWNER, RUN


class APIAcceptance:
    def __init__(self, lab):
        self.lab = lab
        self.output = lab.runner.output
        self.process = None
        self.log = None
        self.base = None
        self.record = {'checks': [], 'cleanup_complete': False}

    def save(self):
        (self.output/'api-test.json').write_text(json.dumps(self.record, indent=2, sort_keys=True)+'\n')

    def mark(self, name):
        self.record['checks'].append({'name': name, 'passed': True})
        self.save()

    def require(self, condition, detail):
        if not condition:
            raise SessionError('api_acceptance_failed', detail)

    def api(self, method, path, token, body=None, expected=None):
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.base+path, method=method, data=data,
                  headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                status = response.status
                value = json.loads(response.read(65536))
        except urllib.error.HTTPError as error:
            status = error.code
            value = json.loads(error.read(65536))
        if expected is not None:
            self.require(status in expected, 'unexpected HTTP status: '+str(status)+' for '+method+' '+path)
        return status, value

    def start(self, binary, credentials, principals, work_ns, pool, instance, attempt):
        log_path = self.output/('api-server-'+str(attempt)+'.jsonl')
        self.log = log_path.open('w')
        args = [str(binary),'--listen','127.0.0.1:0','--kubeconfig',str(credentials),
                '--principals-file',str(principals),'--namespace',work_ns,'--pool',pool,'--instance',instance,
                '--request-timeout','5s','--close-wait','500ms']
        self.process = subprocess.Popen(args,stdin=subprocess.DEVNULL,stdout=self.log,stderr=self.log,
                                        start_new_session=True)
        self.record.setdefault('server_attempts', []).append({'attempt': attempt, 'instance': instance})
        until = time.monotonic()+30
        while time.monotonic()<until:
            self.require(self.process.poll() is None,'API process exited during startup')
            for line in log_path.read_text().splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if event.get('msg') == 'session lifecycle API listening':
                    self.base = 'http://'+event['address']
                    status, value = self.api('GET','/readyz','',expected={200,503})
                    if status == 200 and value.get('ready') is True:
                        return
            time.sleep(.2)
        raise SessionError('api_start_timeout','API did not announce readiness')

    def stop(self):
        if self.process is not None:
            if self.process.poll() is None:
                self.process.terminate()
                try:
                    self.process.wait(timeout=12)
                except subprocess.TimeoutExpired:
                    self.process.kill();self.process.wait(timeout=5)
                    raise SessionError('api_stop_timeout','API did not stop gracefully')
            self.require(self.process.returncode == 0,'API exited unsuccessfully')
            self.process = None
        if self.log is not None:
            self.log.close();self.log = None

    def scoped_credentials(self, directory, work_ns, client_ns):
        prefix=[self.lab.tools['kubectl'],'--kubeconfig',str(self.lab.kubeconfig),'--context','kind-agentbox-platform-lab','--request-timeout=15s']
        # Credential-bearing output is captured privately, never through Runner/report commands.
        token=subprocess.run(prefix+['-n',client_ns,'create','token','agentbox-api','--duration=3600s'],
                             capture_output=True,text=True,timeout=30,check=True).stdout.strip()
        config=json.loads(subprocess.run(prefix+['config','view','--raw','--minify','-o','json'],
                             capture_output=True,text=True,timeout=30,check=True).stdout)
        path=directory/'kubeconfig'
        self.lab.write_json(path,{'apiVersion':'v1','kind':'Config',
          'clusters':[{'name':'lab','cluster':config['clusters'][0]['cluster']}],
          'users':[{'name':'api','user':{'token':token}}],
          'contexts':[{'name':'api','context':{'cluster':'lab','user':'api','namespace':work_ns}}],'current-context':'api'})
        for resource,verb,allowed in [('secrets','get',False),('pods','create',False),('pods','delete',False),
                  ('sandboxwarmpools.extensions.agents.x-k8s.io','get',True),
                  ('sandboxclaims.extensions.agents.x-k8s.io','patch',True),('sandboxclaims.extensions.agents.x-k8s.io','create',True)]:
            result=subprocess.run([self.lab.tools['kubectl'],'--kubeconfig',str(path),'--request-timeout=10s',
                                  '-n',work_ns,'auth','can-i',verb,resource],capture_output=True,text=True,timeout=15)
            self.require(result.stdout.strip()==('yes' if allowed else 'no'),'unexpected scoped API RBAC')
        self.mark('namespace_scoped_credentials_and_no_secret_or_pod_mutation')
        return path

    def wait_state(self, sid, token, wanted, seconds=180):
        until=time.monotonic()+seconds
        while time.monotonic()<until:
            status,value=self.api('GET','/v2/sessions/'+sid,token,expected={200,503})
            if status == 503 and value.get('error') == 'backend_unavailable':
                time.sleep(.5)
                continue
            states = self.record.setdefault('observed_states', [])
            if not states or states[-1].get('state') != value.get('state') or states[-1].get('reason') != value.get('reason'):
                states.append(value)
                self.save()
            if value['state']==wanted:
                return value
            self.require(value['state'] not in ('closed','failed') or wanted==value['state'], 'unexpected terminal session state')
            time.sleep(.5)
        raise SessionError('api_state_timeout','session did not reach '+wanted)

    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);runtime=sessions.attach();bundle=sessions.bundle();sessions.verify(bundle)
            run_id=uuid.uuid4().hex[:10]
            namespaces={}
            self.record.update(run=run_id,owner=sessions.state['session'],namespaces=namespaces,runtime_evidence={})
            binary_dir=self.lab.state_dir/'api-build';binary_dir.mkdir(exist_ok=True)
            binary=binary_dir/'agentbox-api'
            try:
                self.lab.runner.run(['env','CGO_ENABLED=0','go','build','-p=2','-mod=readonly','-trimpath',
                                     '-buildvcs=false','-o',str(binary),'./cmd/agentbox-api'],timeout=900)
                self.record['binary_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
                for role,name in [('work','agentbox-api-'+run_id),('client','agentbox-api-client-'+run_id)]:
                    namespaces[role]=sessions.namespace(name,'api-'+role,run_id)
                work=namespaces['work']['name'];client=namespaces['client']['name']
                params={'OWNER':sessions.state['session'],'RUN':run_id,'WORK_NS':work,'CLIENT_NS':client,
                        'RUNTIME_IMAGE':bundle['images']['runtime']['name']}
                sessions.create(sessions.render('session-template.json',params))
                sessions.create(sessions.render('api-rbac.json',params))
                sessions.create(sessions.render('api-quota.json',params))
                until = time.monotonic() + 180
                while time.monotonic() < until:
                    quota = json.loads(self.lab.kubectl('-n', work, 'get', 'resourcequota', 'agentbox-api-budget', '-o', 'json', announce=False))
                    if quota.get('status', {}).get('hard') == quota['spec']['hard']:
                        self.record['quota'] = quota
                        break
                    time.sleep(.5)
                else:
                    raise SessionError('api_quota_not_ready', 'namespace budget has not been observed by the quota controller')
                with tempfile.TemporaryDirectory(prefix='api-credentials-',dir=self.lab.state_dir) as temporary:
                    directory=Path(temporary);credentials=self.scoped_credentials(directory,work,client)
                    alice,bob=secrets.token_urlsafe(32),secrets.token_urlsafe(32)
                    principals=directory/'principals.json';self.lab.write_json(principals,[{'id':'alice','token':alice},{'id':'bob','token':bob}])
                    self.start(binary,credentials,principals,work,'python-cold','api-'+run_id,1)
                    _,created=self.api('POST','/v2/sessions',alice,{'request_id':'lifecycle-1'},expected={202})
                    sid=created['session_id'];self.record['session_id']=sid
                    _,replayed=self.api('POST','/v2/sessions',alice,{'request_id':'lifecycle-1'},expected={200})
                    self.require(replayed['session_id']==sid and replayed['expires_at']==created['expires_at'],'create replay changed identity or TTL')
                    self.api('POST','/v2/sessions',alice,{'request_id':'lifecycle-1','ttl_seconds':120},expected={409})
                    self.mark('asynchronous_stable_creation_and_parameter_conflict')
                    ready=self.wait_state(sid,alice,'ready');revision=ready['identity_revision'];self.record['ready']=ready
                    claim_name=sid.split('.')[0]
                    claim=json.loads(self.lab.kubectl('-n',work,'get','sandboxclaim',claim_name,'-o','json'))
                    binding=json.loads(claim['metadata']['annotations']['agentbox.dev/api-binding']);self.record['original_binding']=binding
                    pod=json.loads(self.lab.kubectl('-n',work,'get','pod',binding['pod']['name'],'-o','json'))
                    self.record['runtime_evidence'][pod['metadata']['uid']]=runtime.runtime_evidence(pod,memory_bytes=512*1024*1024)
                    self.stop();self.start(binary,credentials,principals,work,'python-cold','api-'+run_id,2)
                    _,restored=self.api('GET','/v2/sessions/'+sid,alice,expected={200})
                    self.require(restored['state']=='ready' and restored['identity_revision']==revision,'restart forgot binding')
                    self.mark('service_restart_restores_persisted_identity')
                    self.api('GET','/v2/sessions/'+sid,bob,expected={404});self.api('DELETE','/v2/sessions/'+sid,bob,expected={404})
                    self.api('GET','/v2/sessions/'+sid+'.wrong',alice,expected={404})
                    self.mark('cross_principal_query_and_close_denied')
                    options={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':binding['pod']['uid']}}
                    self.lab.kubectl('delete','--raw','/api/v1/namespaces/'+work+'/pods/'+binding['pod']['name'],
                                     '-f','-',input_text=json.dumps(options))
                    invalid=self.wait_state(sid,alice,'invalidated',120)
                    self.require(invalid['identity_revision']==revision,'replacement overwrote original binding')
                    until=time.monotonic()+150;replacement=None
                    while time.monotonic()<until:
                        objects=json.loads(self.lab.kubectl('-n',work,'get','pods','-o','json',announce=False))['items']
                        for item in objects:
                            if item['metadata']['uid']!=binding['pod']['uid'] and any(o.get('uid')==binding['sandbox']['uid'] for o in item['metadata'].get('ownerReferences',[])) and any(c['type']=='Ready' and c['status']=='True' for c in item.get('status',{}).get('conditions',[])):
                                replacement=item;break
                        if replacement:break
                        time.sleep(.5)
                    self.require(replacement is not None,'upstream did not recreate Pod')
                    self.record['runtime_evidence'][replacement['metadata']['uid']]=runtime.runtime_evidence(replacement,memory_bytes=512*1024*1024)
                    self.record['replacement_pod_uid']=replacement['metadata']['uid']
                    self.stop();self.start(binary,credentials,principals,work,'python-cold','api-'+run_id,3)
                    _,sticky=self.api('GET','/v2/sessions/'+sid,alice,expected={200})
                    self.require(sticky['state']=='invalidated' and sticky['identity_revision']==revision,'restart re-adopted replacement')
                    self.mark('pod_uid_change_invalidates_without_silent_rebinding')
                    self.api('DELETE','/v2/sessions/'+sid,alice,expected={200,202})
                    closed=self.wait_state(sid,alice,'closed',90);self.require(closed['cleanup_complete'],'closed without cleanup')
                    _,again=self.api('DELETE','/v2/sessions/'+sid,alice,expected={200})
                    self.require(again['state']=='closed','repeated close failed')
                    _,replay=self.api('POST','/v2/sessions',alice,{'request_id':'lifecycle-1'},expected={200})
                    self.require(replay['session_id']==sid and replay['state']=='closed','closed request ID created another environment')
                    self.record['closed']=closed
                    self.mark('close_converges_idempotently_and_retains_creation_record')
                    self.stop()
                # Product cleanup_complete is API-object evidence; the lab verifies runtime independently.
                until=time.monotonic()+60
                while time.monotonic()<until:
                    processes=[p for v in self.record['runtime_evidence'].values() for p in runtime.processes(v['sandbox_id'])]
                    cri=json.loads(runtime.node('crictl','pods','-o','json'))['items']
                    cri=[v for v in cri if v.get('metadata',{}).get('namespace')==work]
                    if not processes and not cri:break
                    time.sleep(.5)
                else:raise SessionError('api_runtime_cleanup_incomplete','runtime still present after product API closed')
                self.record['runtime_cleanup']={'remaining_processes':[],'remaining_cri_sandboxes':[]}
                sessions.close_namespaces(namespaces,run_id)
                self.record.update(cleanup_complete=True,all_passed=True)
                return self.record
            finally:
                try:self.stop()
                finally:self.save()
