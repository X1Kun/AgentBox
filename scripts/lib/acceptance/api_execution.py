"""One real API -> sandboxd -> gVisor vertical flow, plus bounded behavior checks."""

from lib.acceptance.clients.session import SessionClient
import base64
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import secrets
import tempfile
import time
import uuid

from lib.platform.sandbox import Sessions
from lib.core.errors import SessionError


class APIExecutionAcceptance(SessionClient):


    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);runtime=sessions.attach();bundle=sessions.bundle(require_client=False);sessions.verify(bundle)
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
                for name in ('session','api-rbac','api-quota'):
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
                    sessions.create(sessions.render('api',params))
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
