"""One real API -> sandboxd -> gVisor vertical flow, plus bounded behavior checks."""
import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import secrets
import shutil
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

from lib.platform_api import APIAcceptance
from lib.platform_sessions import Sessions, SessionError, checked_source


class APIExecutionAcceptance(APIAcceptance):
    def save(self):
        (self.output/'api-execution-test.json').write_text(json.dumps(self.record,indent=2,sort_keys=True)+'\n')

    def api(self, method, path, token, body=None, expected=None):
        request=urllib.request.Request(self.base+path,method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        try:
            with urllib.request.urlopen(request,context=self.tls_context,timeout=80) as response:
                status=response.status;value=json.loads(response.read(1<<20))
        except urllib.error.HTTPError as error:
            status=error.code;value=json.loads(error.read(1<<20))
        if expected is not None:
            self.require(status in expected,'unexpected HTTP '+str(status)+' for '+method+' '+path+' ('+str(value.get('error',''))+')')
        return status,value

    def build_image(self, sessions):
        directory=self.lab.state_dir/'api-build';directory.mkdir(exist_ok=True)
        binary=directory/'agentbox-api'
        self.lab.runner.run(['env','CGO_ENABLED=0','go','build','-p=2','-mod=readonly','-trimpath',
            '-buildvcs=false','-ldflags=-s -w','-o',binary,'./cmd/agentbox-api'],timeout=900)
        base=checked_source(self.lab.lock['smoke']['image'],sessions.bundle()['python_source'])
        base_image=sessions.docker_image(base)  # Require the verified cached source; no implicit pull.
        self.require(base in base_image.get('RepoDigests',[]),'cached API base digest not confirmed')
        inputs={'binary':hashlib.sha256(binary.read_bytes()).hexdigest(),
                'dockerfile':hashlib.sha256((self.lab.config/'api.Dockerfile').read_bytes()).hexdigest(),
                'base':base,'base_id':base_image['Id']}
        fingerprint=hashlib.sha256(json.dumps(inputs,sort_keys=True).encode()).hexdigest()
        image='agentbox/api:'+fingerprint[:16]
        # Send only the binary and Dockerfile, never previous image archives.
        with tempfile.TemporaryDirectory(prefix='api-image-',dir=self.lab.state_dir) as tmp:
            context=Path(tmp)
            shutil.copyfile(self.lab.config/'api.Dockerfile',context/'Dockerfile')
            shutil.copy2(binary,context/'agentbox-api')
            self.lab.runner.run([self.lab.tools['docker'],'build','--pull=false','--network=none','--build-arg',
                'BASE_IMAGE='+base,'-t',image,context],timeout=300)
        identity=sessions.docker_image(image)['Id']
        node=sessions.state['cluster']+'-control-plane'
        def imported():
            inventory=json.loads(self.lab.runner.run([self.lab.tools['docker'],'exec',node,'crictl','images','-o','json']))['images']
            return any(v['id']==identity and 'docker.io/'+image in v.get('repoTags',[]) for v in inventory)
        cached=imported()
        if not cached:
            processes=self.lab.runner.run([self.lab.tools['docker'],'exec',node,'ps','-eo','comm='])
            self.require('ctr' not in processes.split(),'existing node image import must finish before API image import')
            archive=directory/('api-'+fingerprint[:16]+'.tar')
            self.lab.runner.run([self.lab.tools['docker'],'save','-o',archive,image],timeout=300)
            self.lab.runner.run([self.lab.tools['kind'],'load','image-archive',archive,'--name',sessions.state['cluster'],
                '--nodes',node],timeout=300)
            until=time.monotonic()+60
            while not imported():
                self.require(time.monotonic()<until,'API image ID did not converge in the CRI cache')
                time.sleep(.5)
        self.record['api_image_node_cache_reused']=cached
        self.record['api_image']={'name':image,'id':identity,'inputs':inputs,'fingerprint':fingerprint}
        return image

    def secret(self,directory,client,alice,bob):
        key=directory/'tls.key';cert=directory/'tls.crt'
        self.lab.runner.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj',
            '/CN=agentbox-api-lab','-addext','subjectAltName=IP:127.0.0.1','-keyout',key,'-out',cert],timeout=30)
        values={'tls.key':key.read_bytes(),'tls.crt':cert.read_bytes(),
            'principals.json':json.dumps([{'id':'alice','token':alice},{'id':'bob','token':bob}]).encode()}
        secret={'apiVersion':'v1','kind':'Secret','metadata':{'name':'agentbox-api-credentials','namespace':client},
            'type':'Opaque','data':{k:base64.b64encode(v).decode() for k,v in values.items()}}
        # Never request -o json here: Runner captures stdout, including Secret data.
        self.lab.kubectl('create','-f','-',input_text=json.dumps(secret))
        self.tls_context=ssl.create_default_context(cafile=str(cert))

    def connect(self,client):
        self.lab.kubectl('-n',client,'rollout','status','deployment/agentbox-api','--timeout=180s',timeout=200)
        pods=json.loads(self.lab.kubectl('-n',client,'get','pods','-l','app=agentbox-api','-o','json'))['items']
        self.require(len(pods)==1,'expected exactly one API Pod')
        self.record['api_pod']={'name':pods[0]['metadata']['name'],'uid':pods[0]['metadata']['uid']}
        self.log=(self.output/'api-port-forward.log').open('w')
        self.process=subprocess.Popen([self.lab.tools['kubectl'],'--kubeconfig',str(self.lab.kubeconfig),
            '--context','kind-agentbox-platform-lab','-n',client,'port-forward','--address=127.0.0.1',
            'pod/'+pods[0]['metadata']['name'],':8090'],stdin=subprocess.DEVNULL,stdout=self.log,stderr=self.log)
        until=time.monotonic()+20
        while time.monotonic()<until:
            self.require(self.process.poll() is None,'API port-forward exited')
            for line in (self.output/'api-port-forward.log').read_text().splitlines():
                if line.startswith('Forwarding from 127.0.0.1:'):
                    self.base='https://'+line.split(' ')[2]
                    status,_=self.api('GET','/readyz','',expected={200,503})
                    if status==200:return
            time.sleep(.2)
        raise SessionError('api_connect_timeout','native API Pod port-forward was not ready')

    def stop(self):
        # The forwarded Pod is a trusted ordinary container, not the gVisor Pod.
        if self.process is not None:
            self.process.terminate()
            try:self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:self.process.kill();self.process.wait(timeout=5)
            self.process=None
        if self.log is not None:self.log.close();self.log=None

    def session(self,key,token,runtime,work):
        _,v=self.api('POST','/v2/sessions',token,{'request_id':key},expected={202})
        v=self.wait_state(v['session_id'],token,'ready')
        claim=json.loads(self.lab.kubectl('-n',work,'get','sandboxclaim',v['session_id'].split('.')[0],'-o','json'))
        binding=json.loads(claim['metadata']['annotations']['agentbox.dev/api-binding'])
        pod=json.loads(self.lab.kubectl('-n',work,'get','pod',binding['pod']['name'],'-o','json'))
        self.record['runtime_evidence'][pod['metadata']['uid']]=runtime.runtime_evidence(pod,memory_bytes=512*1024*1024)
        self.record.setdefault('sessions',[]).append(v)
        return v

    def execute(self,v,token,code,expected={200},seconds=10):
        return self.api('POST','/v2/sessions/'+v['session_id']+'/execute',token,
            {'identity_revision':v['identity_revision'],'code':code,'timeout_seconds':seconds},expected)

    def file(self,v,token,path,content=None,expected={200}):
        data={'identity_revision':v['identity_revision'],'path':path}
        action='read'
        if content is not None:action='write';data['content_base64']=base64.b64encode(content).decode()
        return self.api('POST','/v2/sessions/'+v['session_id']+'/files/'+action,token,data,expected)

    def close(self,v,token):
        self.api('DELETE','/v2/sessions/'+v['session_id'],token,expected={200,202})
        self.wait_state(v['session_id'],token,'closed',90)

    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);runtime=sessions.attach();bundle=sessions.bundle();sessions.verify(bundle)
            namespaces={};run=uuid.uuid4().hex[:10]
            self.record.update(run=run,owner=sessions.state['session'],namespaces=namespaces,runtime_evidence={})
            try:
                image=self.build_image(sessions)
                for role in ('work','client'):
                    name='agentbox-exec-'+role+'-'+run
                    namespaces[role]=sessions.namespace(name,'api-'+role,run)
                work=namespaces['work']['name'];client=namespaces['client']['name']
                params={'OWNER':sessions.state['session'],'RUN':run,'WORK_NS':work,'CLIENT_NS':client,
                    'RUNTIME_IMAGE':bundle['images']['runtime']['name'],'API_IMAGE':image}
                for name in ('session-template.json','api-rbac.json','api-quota.json'):
                    sessions.create(sessions.render(name,params))
                sessions.create(sessions.client_policy(client,work,run))
                until=time.monotonic()+120
                while time.monotonic()<until:
                    quota=json.loads(self.lab.kubectl('-n',work,'get','resourcequota','agentbox-api-budget','-o','json',announce=False))
                    if quota.get('status',{}).get('hard')==quota['spec']['hard']:break
                    time.sleep(.5)
                else:raise SessionError('api_quota_not_ready','quota controller has not observed this budget')
                alice,bob=secrets.token_urlsafe(32),secrets.token_urlsafe(32)
                with tempfile.TemporaryDirectory(prefix='api-exec-private-',dir=self.lab.state_dir) as tmp:
                    # Keep an independent live check of the namespace-scoped RBAC.
                    self.scoped_credentials(Path(tmp),work,client)
                    self.secret(Path(tmp),client,alice,bob)
                    sessions.create(sessions.render('api-deployment.json',params))
                    self.connect(client)
                    a=self.session('analysis',alice,runtime,work)
                    self.file(a,alice,'input.csv',b'value\n2\n3\n5\n')
                    code="import csv,json\nrows=list(csv.DictReader(open('input.csv')))\ntotal=sum(int(r['value']) for r in rows)\njson.dump({'total':total,'count':len(rows)},open('report.json','w'))\nprint(total)"
                    self.file(a,alice,'analysis.py',code.encode())
                    _,result=self.execute(a,alice,"exec(open('analysis.py').read())")
                    self.require(result['exit_code']==0 and result['stdout'].strip()=='10','analysis did not execute')
                    _,file=self.file(a,alice,'report.json')
                    self.require(json.loads(base64.b64decode(file['content_base64']))=={'total':10,'count':3},'wrong artifact')
                    _,second=self.execute(a,alice,"import json\nr=json.load(open('report.json'))\nprint(r['total']+1)\nprint('total' in globals())")
                    self.require(second['stdout'].splitlines()==['11','False'],'files did not persist or interpreter leaked')
                    self.mark('csv_script_execution_artifact_and_incremental_call')
                    b=self.session('isolated',alice,runtime,work)
                    self.file(b,alice,'report.json',expected={404})
                    self.file(b,alice,'binary.bin',b'\x00\xff\x01')
                    _,raw=self.file(b,alice,'binary.bin');self.require(base64.b64decode(raw['content_base64'])==b'\x00\xff\x01','binary artifact changed')
                    self.execute(a,bob,'print(1)',expected={404})
                    self.file(a,bob,'report.json',expected={404})
                    self.api('POST','/v2/sessions/'+a['session_id']+'/execute',alice,
                        {'identity_revision':'old','code':'print(1)'},expected={409})
                    self.mark('session_and_principal_isolation_binary_files_and_revision_guard')
                    _,failure=self.execute(a,alice,"raise ValueError('expected')")
                    self.require(failure['exit_code']!=0 and 'ValueError' in failure['stderr'],'nonzero exit lost')
                    self.execute(a,alice,"print('recovered')")
                    self.file(a,alice,'../outside',expected={400})
                    self.execute(a,alice,"import os\nos.symlink('/etc/passwd','escape')")
                    self.file(a,alice,'escape',expected={403})
                    self.execute(a,alice,"print('still ready')")
                    self.mark('program_failure_recovery_and_workspace_boundary')
                    # One active command; files and a second command reject, while close still works.
                    with ThreadPoolExecutor(max_workers=1) as pool:
                        active=pool.submit(self.execute,a,alice,"import time\nprint('started',flush=True)\ntime.sleep(30)",{503},45)
                        until=time.monotonic()+15
                        while time.monotonic()<until:
                            _,state=self.api('GET','/v2/sessions/'+a['session_id'],alice,expected={200})
                            if state['operation_active']:break
                            time.sleep(.2)
                        else:raise SessionError('api_execution_not_active','execution did not acquire its durable marker')
                        self.execute(a,alice,'print(2)',expected={409})
                        self.file(a,alice,'input.csv',expected={409})
                        self.close(a,alice)
                        _,unknown=active.result(timeout=15)
                        self.require(unknown['error']=='operation_outcome_unknown','close race fabricated an execution result')
                    self.execute(a,alice,'print(3)',expected={409})
                    self.close(b,alice)
                    self.mark('concurrent_operation_conflicts_and_close_running_session')
                    timed=self.session('timeout',alice,runtime,work)
                    _,failure=self.execute(timed,alice,"import time\ntime.sleep(30)",expected={504},seconds=1)
                    self.require(failure['error']=='execution_timeout','timeout classification wrong')
                    self.wait_state(timed['session_id'],alice,'closed',90)
                    self.mark('execution_timeout_requests_full_session_cleanup_without_replay')
                    large=self.session('output-limit',alice,runtime,work)
                    _,failure=self.execute(large,alice,"print('x'*70000)",expected={413})
                    self.require(failure['error']=='execution_output_limit','output limit missing')
                    self.wait_state(large['session_id'],alice,'closed',90)
                    self.mark('bounded_output_closes_session')
                    self.stop()
                until=time.monotonic()+60
                while time.monotonic()<until:
                    processes=[p for v in self.record['runtime_evidence'].values() for p in runtime.processes(v['sandbox_id'])]
                    cri=json.loads(runtime.node('crictl','pods','-o','json'))['items']
                    cri=[v for v in cri if v.get('metadata',{}).get('namespace')==work]
                    if not processes and not cri:break
                    time.sleep(.5)
                else:raise SessionError('api_execution_cleanup_incomplete','runtime still present after API cleanup')
                self.record['runtime_cleanup']={'remaining_processes':[],'remaining_cri_sandboxes':[]}
                sessions.close_namespaces(namespaces,run)
                self.record.update(cleanup_complete=True,all_passed=True,development_cluster_retained=True)
                return self.record
            finally:
                self.stop();self.save()
