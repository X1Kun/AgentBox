"""One bounded lifecycle API behavior test with real namespace-scoped credentials."""

import json
import subprocess
import time
import urllib.error
import urllib.request

from lib.core.errors import SessionError


class LifecycleClient:
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


    def api(self, method, path, token, body=None, expected=None, request_timeout=10):
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.base+path, method=method, data=data,
                  headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        try:
            with urllib.request.urlopen(request, timeout=request_timeout) as response:
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
            status,value=self.api('GET','/v2/sessions/'+sid,token,expected={200,503,504}, request_timeout=min(10, max(.01, until-time.monotonic())))
            # Read-only polling may recover transient dependency failures within
            # the original observation budget; mutations are never replayed.
            if (status, value.get('error')) in ((503, 'backend_unavailable'), (503, 'backend_throttled'), (504, 'backend_timeout')):
                time.sleep(.5)
                continue
            self.require(status == 200, 'state query rejected: ' + str(value.get('error')))
            states = self.record.setdefault('observed_states', [])
            if not states or states[-1].get('state') != value.get('state') or states[-1].get('reason') != value.get('reason'):
                states.append(value)
                self.save()
            if value['state']==wanted:
                return value
            self.require(value['state'] not in ('closed','failed') or wanted==value['state'], 'unexpected terminal session state')
            time.sleep(.5)
        raise SessionError('api_state_timeout','session did not reach '+wanted)


