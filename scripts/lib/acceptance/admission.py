"""Real Kueue health admission, faults, leader takeover and session signal wiring."""
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import urllib.parse
import uuid

from lib.acceptance.clients.batch_environment import BatchEnvironment
from lib.acceptance.clients.session import SessionClient
from lib.core.kubernetes import identity
from lib.core.validation import require
from lib.manifests import read_yaml
from lib.observability.environment import instrument
from lib.observability.session import SessionEnvironment
from lib.platform.sandbox import Sessions
from lib.product.admission import compose
from lib.product.worker_image import worker_bundle


class HealthFixture:
    """Only metrics are injected; every query passes through real Prometheus."""
    def __init__(self, output, revision):
        self.mode, self.revision, self.started = 'bad', revision, time.time()-30
        self.lock, self.history = threading.Lock(), []
        fixture = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_GET(self):
                with fixture.lock:
                    mode = fixture.mode
                if self.path.startswith('/api/v1/query?'):
                    if mode == 'query-denied':
                        self.send_error(403); return
                    try:
                        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                        with opener.open('http://127.0.0.1:19090'+self.path, timeout=5) as response:
                            body = response.read(256 << 10)
                        self.send_response(200); self.end_headers(); self.wfile.write(body)
                    except (OSError, urllib.error.URLError):
                        self.send_error(503)
                    return
                if self.path != '/metrics': self.send_error(404); return
                now = int(time.time())
                end = now-2 if mode != 'stale' else int(fixture.started)+2
                fields = {'source_started_seconds':fixture.started, 'window_end_seconds':end,
                          'sequence':now if mode != 'stale' else 1, 'window_seconds':2,
                          'deadline_seconds':1, 'planned':100, 'timely_correct':100 if mode=='good' else 0,
                          'errors':0 if mode=='good' else 100, 'timeouts':0, 'rejected':0, 'valid':1}
                if mode == 'empty': fields.update(planned=0, timely_correct=0, errors=0)
                with fixture.lock:
                    fixture.history.append({'at':time.time(), 'mode':mode, 'fields':fields})
                lines = [f'agentbox_session_health{{domain="worker",revision="{fixture.revision}",profile="short-followup",field="{key}"}} {value}'
                         for key,value in fields.items()]
                body = ('\n'.join(lines)+'\n').encode()
                self.send_response(200)
                self.send_header('Content-Type','text/plain; version=0.0.4; charset=utf-8')
                self.end_headers(); self.wfile.write(body)
        self.server = ThreadingHTTPServer(('127.0.0.1',0), Handler)
        self.server.daemon_threads = True
        self.address = '127.0.0.1:'+str(self.server.server_port)
        self.thread = threading.Thread(target=self.server.serve_forever,daemon=True)
        self.thread.start()
        self.output = output

    def set(self, mode):
        with self.lock: self.mode=mode

    def close(self):
        self.server.shutdown();self.server.server_close();self.thread.join(timeout=5)
        self.output.write_text(''.join(json.dumps(row)+'\n' for row in self.history))


def free_address():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1',0));return '127.0.0.1:'+str(sock.getsockname()[1])


class AdmissionAcceptance(SessionEnvironment):
    def __init__(self, lab):
        super().__init__(lab)
        self.controllers, self.resources, self.controller_logs = [], [], []

    def save(self): self.lab.write_json(self.output/'admission-test.json',self.record)

    def api_test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);sessions.attach()
            product=BatchEnvironment(self.lab);product.kueue.attach();product.kueue.verify()
            run=uuid.uuid4().hex[:10];owner=sessions.state['session'];namespace='agentbox-admission-api-'+run
            self.record.update(run=run,owner=owner,scope='Real status subresource CAS only; no computation or performance measurement')
            try:
                self.create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':namespace,'labels':{'agentbox.dev/session':owner}}})
                self.lab.runner.run(['env','AGENTBOX_ADMISSION_TEST_KUBECONFIG='+str(self.lab.kubeconfig),
                    'AGENTBOX_ADMISSION_TEST_NAMESPACE='+namespace,'AGENTBOX_ADMISSION_TEST_OWNER='+owner,
                    'go','test','-race','-tags=integration','-run','^TestRealStatusCAS','-count=3','./internal/admission'],timeout=180)
                self.mark('three_real_apiserver_status_cas_runs')
                self.record['all_passed']=True
            finally:
                self.cleanup_controller();self.record['cleanup_complete']=True;self.save()
            return self.record

    def create(self, obj):
        value=json.loads(self.lab.kubectl('create','-f','-','-o','json',input_text=json.dumps(obj),announce=False))
        self.resources.append({'apiVersion':value['apiVersion'],'kind':value['kind'],
            'name':value['metadata']['name'],'namespace':value['metadata'].get('namespace'),
            'uid':value['metadata']['uid']})
        self.record['controller_resources']=self.resources;self.save();return value

    def wait_for(self, predicate, reason, timeout=90):
        until=time.monotonic()+timeout
        while time.monotonic()<until:
            require(not self.controllers or any(p.poll() is None for p in self.controllers),
                    'admission_controller_stopped','see controller JSONL')
            result=predicate()
            if result:return result
            time.sleep(.5)
        require(False,reason,'see admission-test.json and controller events')

    def workloads(self, product):
        return json.loads(self.lab.kubectl('-n',product.record['namespace'],'get','workloads','-o','json',announce=False))['items']

    def check_state(self, product, view, state):
        uid=view['batch_id'].split('.')[1]
        for obj in self.workloads(product):
            if any(r.get('uid')==uid for r in obj['metadata'].get('ownerReferences',[])):
                for check in obj.get('status',{}).get('admissionChecks',[]):
                    if check['name']==self.policy['admission_check'] and check['state']==state:
                        if state=='Pending' and not any(c['type']=='QuotaReserved' and c['status']=='True'
                                for c in obj.get('status',{}).get('conditions',[])):
                            continue
                        self.record.setdefault('workload_observations',[]).append(obj);self.save();return obj
        return None

    def patch(self, kind, name, path, value, namespace=None, status=False):
        args=['-n',namespace] if namespace else []
        obj=json.loads(self.lab.kubectl(*args,'get',kind,name,'-o','json',announce=False))
        require(any(r['uid']==obj['metadata']['uid'] for r in self.resources) or
                obj['metadata'].get('labels',{}).get('agentbox.dev/batch-run')==self.record['run'],
                'unowned_admission_fault',name)
        operations=[{'op':'test','path':'/metadata/uid','value':obj['metadata']['uid']},
                    {'op':'test','path':'/metadata/resourceVersion','value':obj['metadata']['resourceVersion']},
                    {'op':'add','path':path,'value':value}]
        command=['patch',kind,name,'--type=json','-p',json.dumps(operations)]
        if status:command+=['--subresource=status']
        return json.loads(self.lab.kubectl(*args,*command,'-o','json',announce=False))

    def credentials(self, private, namespace):
        prefix=[self.lab.tools['kubectl'],'--kubeconfig',str(self.lab.kubeconfig),'--context','kind-agentbox-platform-lab']
        token=subprocess.run(prefix+['-n',namespace,'create','token','agentbox-admission','--duration=3600s'],
                             capture_output=True,text=True,timeout=30,check=True).stdout.strip()
        raw=json.loads(subprocess.run(prefix+['config','view','--raw','--minify','-o','json'],
                             capture_output=True,text=True,timeout=20,check=True).stdout)
        path=private/'controller-kubeconfig'
        self.lab.write_json(path,{'apiVersion':'v1','kind':'Config','clusters':[{'name':'lab','cluster':raw['clusters'][0]['cluster']}],
            'users':[{'name':'controller','user':{'token':token}}],
            'contexts':[{'name':'controller','context':{'cluster':'lab','user':'controller'}}],'current-context':'controller'})
        path.chmod(0o600)
        for resource,verb in [('secrets','get'),('jobs','patch'),('pods','delete'),('sandboxclaims.extensions.agents.x-k8s.io','create')]:
            command=[self.lab.tools['kubectl'],'--kubeconfig',str(path),'-n',self.policy['namespace'],'auth','can-i',verb,resource]
            answer=subprocess.run(command,capture_output=True,text=True,timeout=15)
            require(answer.stdout.strip()=='no','admission_rbac_excess',resource)
        self.mark('separate_controller_identity_no_task_or_secret_permissions')
        return path

    def start_controller(self, binary, credentials, config, namespace):
        index=len(self.controllers);metrics,probes=free_address(),free_address()
        log=(self.output/f'controller-{index}.jsonl').open('w')
        process=subprocess.Popen([str(binary),'--config',str(config),'--kubeconfig',str(credentials),
            '--lease-namespace',namespace,'--metrics-listen',metrics,'--probe-listen',probes],
            stdin=subprocess.DEVNULL,stdout=log,stderr=log)
        self.controllers.append(process);self.controller_logs.append(log)
        return process,metrics

    def stop_controller(self, process):
        if process.poll() is None:
            process.terminate()
            try:process.wait(timeout=20)
            except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)

    def policy_file(self, path):
        temporary=path.with_suffix('.tmp');self.lab.write_json(temporary,self.policy);temporary.replace(path)

    def submit(self, product, name, passes=1):
        _,view=product.api('POST','/v2/batches',product.alice,
            {'request_id':name,'dataset_id':product.record['worker']['dataset'],'passes':passes},expected={200,202})
        return view

    def held(self, product, view):
        value=self.wait_for(lambda:self.check_state(product,view,'Pending'),'admission_not_pending')
        require(not any(c['type']=='Admitted' and c['status']=='True' for c in value['status']['conditions']),
                'unhealthy_admitted','workload admitted before permit')
        name=view['batch_id'].split('.')[0]
        require(product.get('job',name,product.record['namespace'])['spec']['suspend'],
                'unhealthy_job_started','unhealthy Job must stay suspended')
        return value

    def retried_without_quota(self, product, view):
        uid=view['batch_id'].split('.')[1]
        for workload in self.workloads(product):
            if any(r.get('uid')==uid for r in workload['metadata'].get('ownerReferences',[])):
                retry=next((c for c in workload.get('status',{}).get('admissionChecks',[]) if c['name']==self.policy['admission_check']),{})
                if retry.get('retryCount',0)>0 and not workload.get('status',{}).get('admission'):
                    self.record['native_retry_observation']=workload;self.save();return workload
        return None

    def archive_control_metrics(self):
        series={}
        for name in ('agentbox_session_health','agentbox_admission_state','agentbox_admission_sample_valid',
                     'agentbox_admission_actions_total','agentbox_admission_decisions_total',
                     'agentbox_admission_reconcile_timestamp_seconds'):
            query='/api/v1/query_range?'+urllib.parse.urlencode({'query':name,'start':self.record['started_at'],
                                                               'end':time.time(),'step':5})
            series[name]=json.loads(self.stack.http(19090,query))
            require(series[name].get('status')=='success','admission_metrics_archive_failed',name)
        self.lab.write_json(self.output/'admission-metrics.json',series)

    def cleanup_controller(self):
        for process in self.controllers:self.stop_controller(process)
        for log in self.controller_logs:log.close()
        paths={'AdmissionCheck':('/apis/kueue.x-k8s.io/v1beta2/admissionchecks/',False),
               'ClusterRoleBinding':('/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/',False),
               'ClusterRole':('/apis/rbac.authorization.k8s.io/v1/clusterroles/',False),
               'Namespace':('/api/v1/namespaces/',False)}
        for kind in ('AdmissionCheck','ClusterRoleBinding','ClusterRole','Namespace'):
            for ref in (r for r in self.resources if r['kind']==kind):
                actual=json.loads(self.lab.kubectl('get',kind,ref['name'],'--ignore-not-found','-o','json',announce=False) or 'null')
                if not actual:continue
                require(actual['metadata']['uid']==ref['uid'],'admission_cleanup_identity_changed',ref['name'])
                options={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':ref['uid']}}
                self.lab.kubectl('delete','--raw',paths[kind][0]+ref['name'],'-f','-',input_text=json.dumps(options),announce=False)
                self.lab.kubectl('wait','--for=delete',kind+'/'+ref['name'],'--timeout=90s',timeout=95,announce=False)

    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);runtime=sessions.attach();bundle=sessions.bundle(require_client=False);sessions.verify(bundle)
            api,product=SessionClient(self.lab),BatchEnvironment(self.lab)
            product.runtime,product.state=runtime,sessions.state;product.kueue.attach();product.kueue.verify()
            run=uuid.uuid4().hex[:10];owner=sessions.state['session'];controller_ns='agentbox-admission-'+run
            self.record.update(run=run,owner=owner,started_at=time.time(),scope='P4 functional/fault acceptance; no R/B/A performance qualification')
            api.record.update(run=run,owner=owner,runtime_evidence={})
            fixture=None;captured={};driver=None
            try:
                self.stack.up();self.stack.reload_prometheus()
                fixture=HealthFixture(self.output/'injected-health.jsonl','fixture-'+run)
                image=api.build_image(sessions);worker=worker_bundle(self.lab)
                binary=self.lab.state_dir/'agentbox-admission'
                self.lab.runner.run(['go','build','-trimpath','-buildvcs=false','-o',binary,'./cmd/agentbox-admission'],timeout=900)
                with tempfile.TemporaryDirectory(prefix='admission-private-',dir=self.lab.state_dir) as tmp:
                    private=Path(tmp);batch_private=private/'batch';batch_private.mkdir()
                    product.start(sessions,runtime,image,worker,batch_private,run)
                    self.policy=read_yaml(self.lab.config/'agentbox/admission/policy.example.yaml')
                    self.policy.update(namespace=product.record['namespace'],cluster_queue=product.record['namespace'],
                        admission_check='agentbox-health-'+run,owner=owner,prometheus_url='http://'+fixture.address,
                        minimum_samples=2,max_age_seconds=10,stable_seconds=4,protect_seconds=2,
                        permit_interval_seconds=6,max_wait_seconds=40,retry_seconds=5,poll_seconds=1,request_timeout_seconds=5)
                    self.policy['health'].update(revision=fixture.revision,window_seconds=2,deadline_seconds=1)
                    deployment_policy=dict(self.policy,prometheus_url='https://prometheus.fixture.test:19090')
                    rendered=compose(self.lab.config,deployment_policy,'agentbox/admission@sha256:'+'1'*64,controller_ns,
                        [{'address':'10.254.0.1','port':443}],[{'address':'10.254.0.2','port':19090}])
                    for obj in rendered:
                        if obj['kind'] not in ('Deployment','ConfigMap','NetworkPolicy'):self.create(obj)
                    self.record['rendered_resource_kinds']=[o['kind'] for o in rendered]
                    credentials=self.credentials(private,controller_ns);config=private/'policy.json';self.policy_file(config)
                    first,metrics0=self.start_controller(binary,credentials,config,controller_ns)
                    self.stack.control_targets({'health':fixture.address,'admission-0':metrics0})
                    check=self.policy['admission_check']
                    self.wait_for(lambda:any(c['type']=='Active' and c['status']=='True' for c in
                        product.get('admissioncheck',check).get('status',{}).get('conditions',[])),'admission_controller_inactive')
                    attached=self.patch('clusterqueue',product.record['namespace'],'/spec/admissionChecksStrategy',{'admissionChecks':[{'name':check}]})
                    require(attached['spec'].get('admissionChecksStrategy')=={'admissionChecks':[{'name':check}]},
                            'admission_attachment_pruned','v1beta2 strategy must survive server validation')
                    product.quota(1)
                    pending=self.submit(product,'retry-then-complete',passes=8);self.held(product,pending)
                    self.mark('unhealthy_workload_reserves_quota_and_stays_suspended')
                    self.wait_for(lambda:self.retried_without_quota(product,pending),
                        'native_retry_not_observed',timeout=100)
                    self.mark('pending_timeout_native_retry_and_backoff')
                    fixture.set('good')
                    completed=product.wait_task(pending,'succeeded');product.verify_artifacts(completed)
                    self.mark('healthy_recovery_real_image_job_and_eight_verified_pngs')
                    product.api('DELETE','/v2/batches/'+pending['batch_id'],product.alice,expected={200,202})
                    product.wait_task(pending,'succeeded',cleanup=True)
                    fixture.set('stale')
                    self.wait('agentbox_admission_sample_valid == 0')
                    blocked=self.submit(product,'stale-blocked');self.held(product,blocked)
                    self.mark('stale_source_does_not_mean_idle')
                    fixture.set('query-denied');self.wait('agentbox_admission_state{state="degraded"} == 1')
                    self.held(product,blocked);self.mark('query_permission_failure_stops_new_permissions')
                    fixture.set('empty');self.wait('agentbox_admission_decisions_total{reason="insufficient_samples"} >= 1')
                    self.held(product,blocked);self.mark('zero_requests_do_not_mean_healthy')
                    role=product.get('role','agentbox-admission',product.record['namespace'])
                    original=role['rules']
                    restricted=[r for r in original if r.get('resources')!=['workloads/status']]
                    try:
                        self.patch('role','agentbox-admission','/rules',restricted,product.record['namespace'])
                        fixture.set('good')
                        self.wait('agentbox_admission_actions_total{action="Ready",outcome="unknown"} >= 1')
                        self.held(product,blocked)
                        self.mark('status_permission_loss_does_not_fail_open')
                    finally:
                        current=product.get('role','agentbox-admission',product.record['namespace'])
                        require(current['metadata']['uid']==role['metadata']['uid'] and current['rules']==restricted,
                                'admission_role_changed','refusing to overwrite external RBAC changes')
                        self.patch('role','agentbox-admission','/rules',original,product.record['namespace'])
                    fixture.set('bad')
                    self.wait('agentbox_admission_state{state="protect",instance="admission-0"} == 1')
                    product.quota(2)
                    paced=self.submit(product,'paced-after-leader-change');self.held(product,paced)
                    # Two real managers share one restricted Lease and original objects.
                    second,metrics1=self.start_controller(binary,credentials,config,controller_ns)
                    self.stack.control_targets({'health':fixture.address,'admission-0':metrics0,'admission-1':metrics1})
                    self.wait('up{job="agentbox-control",instance="admission-1"} == 1')
                    self.stop_controller(first)
                    fixture.set('good')
                    self.wait_for(lambda:second.poll() is None and self.check_state(product,blocked,'Ready'),
                                  'leader_takeover_did_not_recover',timeout=100)
                    completed=product.wait_task(blocked,'succeeded');product.verify_artifacts(completed)
                    completed2=product.wait_task(paced,'succeeded');product.verify_artifacts(completed2)
                    granted=[self.check_state(product,v,'Ready') for v in (blocked,paced)]
                    times=sorted(datetime.fromisoformat(next(c['lastTransitionTime'] for c in w['status']['admissionChecks'] if c['name']==check).replace('Z','+00:00')).timestamp() for w in granted)
                    require(times[1]-times[0]>=self.policy['permit_interval_seconds']-.01,
                            'admission_recovery_burst','grants violated configured interval')
                    self.record['recovery_grant_interval_seconds']=times[1]-times[0]
                    self.mark('leader_takeover_rebuilds_health_and_recovers_same_workload')
                    product.api('DELETE','/v2/batches/'+blocked['batch_id'],product.alice,expected={200,202})
                    product.wait_task(blocked,'succeeded',cleanup=True)
                    product.api('DELETE','/v2/batches/'+paced['batch_id'],product.alice,expected={200,202})
                    product.wait_task(paced,'succeeded',cleanup=True)
                    product.quota(1)
                    # A long bounded image workload makes in-flight protection observable.
                    running=self.submit(product,'running-protection',passes=4096)
                    pod=product.wait_running(running)
                    fixture.set('bad')
                    self.wait('agentbox_admission_state{state="protect",instance="admission-1"} == 1')
                    require(self.check_state(product,running,'Ready'),'running_permit_revoked','Ready must remain durable')
                    job=product.get('job',running['batch_id'].split('.')[0],product.record['namespace'])
                    require(job['metadata']['uid']==running['batch_id'].split('.')[1] and not job['spec']['suspend'],
                            'inflight_evicted','health extension may not evict an admitted Job')
                    product.api('DELETE','/v2/batches/'+running['batch_id'],product.alice,expected={200,202})
                    product.wait_task(running,'cancelled',cleanup=True);product.wait_runtime_cleanup(running['batch_id'].split('.')[0])
                    self.mark('health_deterioration_preserves_running_job_and_explicit_cancel_reclaims_runtime')
                    # Real API status/UID conflict tests use a disposable non-compute namespace.
                    env={'AGENTBOX_ADMISSION_TEST_KUBECONFIG':str(self.lab.kubeconfig),
                         'AGENTBOX_ADMISSION_TEST_NAMESPACE':controller_ns,'AGENTBOX_ADMISSION_TEST_OWNER':owner}
                    self.lab.runner.run(['env',*[k+'='+v for k,v in env.items()],'go','test','-race','-tags=integration',
                        '-run','^TestRealStatusCAS','-count=1','./internal/admission'],timeout=180)
                    self.mark('real_apiserver_status_concurrency_and_uid_protection')
                    self.real_signal(sessions,runtime,bundle,api,product,image,private,fixture,config,captured)
                    self.record['all_passed']=True;self.save()
            finally:
                primary=sys.exc_info()[1]
                if primary:self.record['primary_failure']={'type':type(primary).__name__,'reason':getattr(primary,'reason',None)}
                self.save()
                errors=[]
                for stage,action in [('control_metrics_archive',self.archive_control_metrics),
                                     ('control_targets_clear',lambda:self.stack.control_targets({})),
                                     ('controllers_stop',lambda:[self.stop_controller(p) for p in self.controllers]),
                                     ('batch_teardown',lambda:product.teardown() if product.record.get('namespace') else None),
                                     ('controller_resources',self.cleanup_controller),
                                     ('session_teardown',lambda:self._teardown(api,sessions,runtime,captured,run))]:
                    try:action()
                    except Exception as error:errors.append({'stage':stage,'type':type(error).__name__,'reason':getattr(error,'reason',None)})
                if fixture:fixture.close()
                self.record['admission_cleanup_errors']=errors
                self.record['cleanup_complete']=not errors and product.record.get('cleanup_complete',True) and self.record.get('cleanup_complete',not self.namespaces)
                self.record['all_passed']=self.record['all_passed'] and self.record['cleanup_complete'];self.save()
            return self.record

    def real_signal(self,sessions,runtime,bundle,api,product,image,private,fixture,config,captured):
        run=self.record['run']
        for role in ('work','client'):
            self.namespaces[role]=sessions.namespace('agentbox-health-'+role+'-'+run,'health-'+role,run)
        self.record['namespaces']=self.namespaces;self.save()
        work,client=self.namespaces['work']['name'],self.namespaces['client']['name']
        params={'OWNER':self.record['owner'],'RUN':run,'WORK_NS':work,'CLIENT_NS':client,
                'RUNTIME_IMAGE':bundle['images']['runtime']['name'],'API_IMAGE':image}
        for name in ('session-warm','api-rbac','api-quota'):
            sessions.create(sessions.render(name,params))
        sessions.create(sessions.client_policy(client,work,run))
        session_private=private/'session';session_private.mkdir()
        token=secrets.token_urlsafe(32);api.secret(session_private,client,token,secrets.token_urlsafe(32))
        deployment=instrument(self.lab,sessions.render('api',params));args=deployment['spec']['template']['spec']['containers'][0]['args']
        args[args.index('--pool=python-cold')]='--pool=python-warm';sessions.create(deployment)
        api.connect(client)
        views=[]
        for i in range(2):
            view=api.session('health-'+str(i),token,runtime,work);views.append(view)
            _,warm=api.execute(view,token,'print(42)',expected={200});require(warm['stdout']=='42\n','health_warmup_failed','unexpected output')
        captured.update(api.record['runtime_evidence'])
        self.policy['health'].update(revision='live-'+run,window_seconds=10,deadline_seconds=10)
        self.policy.update(minimum_samples=1,healthy_ratio=.5,protect_ratio=.1,max_age_seconds=30,stable_seconds=10,max_wait_seconds=120)
        self.patch('admissioncheck',self.policy['admission_check'],'/metadata/annotations/agentbox.dev~1policy-revision',self.policy['health']['revision'])
        self.policy_file(config)
        driver_binary=self.lab.state_dir/'agentbox-session-experiment'
        self.lab.runner.run(['go','build','-trimpath','-buildvcs=false','-o',driver_binary,'./cmd/agentbox-session-experiment'],timeout=900)
        trace={'health':self.policy['health'],'start_at':datetime.fromtimestamp(time.time()+5,timezone.utc).isoformat(),
            'targets':[{'id':v['session_id'],'revision':v['identity_revision']} for v in views],
            'max_inflight':8,'max_lag_ms':1000,'deadline_ms':10000,
            'requests':[{'id':'followup-'+str(i),'at_ms':i*4000,'target':i%2,'action':'execute',
                'code':'print(sum(range(1000)))','timeout_seconds':20,'expected_out':'499500\n','expected_exit':0} for i in range(15)]}
        trace_path=session_private/'trace.json';self.lab.write_json(trace_path,trace)
        address=free_address();out=(self.output/'health-driver-summary.json').open('w');err=(self.output/'health-driver.log').open('w')
        driver=subprocess.Popen([str(driver_binary),'--config',str(trace_path),'--ledger',str(self.output/'health-session.jsonl'),
            '--api-url',api.base,'--api-ca-file',str(session_private/'tls.crt'),'--health-listen',address,'--health-linger','30s'],
            env={**os.environ,'AGENTBOX_API_TOKEN':token},stdout=out,stderr=err)
        try:
            # The controller queries the same real Prometheus, now selecting a
            # different revision produced only by the real Go session driver.
            self.stack.control_targets({'health':address})
            pending=self.submit(product,'live-session-health')
            completed=product.wait_task(pending,'succeeded',seconds=150);product.verify_artifacts(completed)
            driver.wait(timeout=160)
            require(driver.returncode==0,'health_driver_failed','see private-free driver logs')
            events=[json.loads(line) for line in (self.output/'health-session.jsonl').read_text().splitlines()]
            require(len(events)==15 and any(e['outcome']=='correct' and e['timely'] for e in events),
                    'health_signal_not_verified','missing correct timely commands')
            self.record['real_health_signal']={'planned':len(events),'correct':sum(e['outcome']=='correct' for e in events),
                'timely':sum(e['timely'] for e in events),'contract':self.policy['health'],'qualified_baseline':False}
            self.mark('real_followup_driver_prometheus_controller_job_and_pngs')
            product.api('DELETE','/v2/batches/'+pending['batch_id'],product.alice,expected={200,202})
            product.wait_task(pending,'succeeded',cleanup=True)
        finally:
            if driver.poll() is None:driver.terminate();driver.wait(timeout=10)
            out.close();err.close()
        for view in views:api.close(view,token)
