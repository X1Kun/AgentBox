"""One real API -> sandboxd -> gVisor vertical flow, plus bounded behavior checks."""

import base64
import json
import ssl
import subprocess
import time
import urllib.error
import urllib.request

from lib.product.api_image import build_api_image
from lib.core.errors import SessionError


from lib.acceptance.clients.lifecycle import LifecycleClient

class SessionClient(LifecycleClient):
    def save(self):
        (self.output/'api-execution-test.json').write_text(json.dumps(self.record,indent=2,sort_keys=True)+'\n')


    def api(self, method, path, token, body=None, expected=None, request_timeout=80):
        request=urllib.request.Request(self.base+path,method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        try:
            with urllib.request.urlopen(request,context=self.tls_context,timeout=request_timeout) as response:
                status=response.status;value=json.loads(response.read(1<<20))
        except urllib.error.HTTPError as error:
            status=error.code;value=json.loads(error.read(1<<20))
        if expected is not None:
            self.require(status in expected,'unexpected HTTP '+str(status)+' for '+method+' '+path+' ('+str(value.get('error',''))+')')
        return status,value


    def build_image(self, sessions):
        prepared = build_api_image(self.lab, sessions)
        self.record.update(prepared)
        return prepared['api_image']['name']


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
        # Creation has a stable identity; recover only that exact request. This
        # never authorizes retry of execute/write or extends the startup window.
        until = time.monotonic() + 180
        while True:
            self.require(time.monotonic() < until, 'creation recovery budget exhausted')
            status, v = self.api('POST', '/v2/sessions', token, {'request_id': key}, expected={200, 202, 503, 504}, request_timeout=min(80, max(.01, until-time.monotonic())))
            if status in (200, 202):
                break
            self.require(v.get('error') in ('backend_timeout', 'backend_unavailable', 'backend_throttled', 'create_outcome_unknown'),
                         'creation rejected: ' + str(v.get('error')))
            self.record.setdefault('creation_recoveries', []).append({'request_id': key, 'error': v.get('error')})
            self.save()
            self.require(time.monotonic() < until, 'creation recovery budget exhausted')
            time.sleep(.5)
        v=self.wait_state(v['session_id'],token,'ready',max(0, until-time.monotonic()))
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
        until = time.monotonic() + 90
        while time.monotonic() < until:
            status, response = self.api('DELETE', '/v2/sessions/' + v['session_id'], token,
                                        expected={200, 202, 503, 504}, request_timeout=min(80, max(.01, until-time.monotonic())))
            if status in (200, 202):
                self.wait_state(v['session_id'],token,'closed',max(0, until-time.monotonic()))
                return
            self.require(response.get('error') in ('backend_unavailable', 'backend_timeout', 'backend_throttled'),
                         'closure rejected: ' + str(response.get('error')))
            self.record.setdefault('close_recoveries', []).append({'session_id': v['session_id'], 'error': response.get('error')})
            self.save()
            time.sleep(.5)
        self.require(False, 'closure recovery budget exhausted')


