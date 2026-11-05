"""One bounded lifecycle API behavior test with real namespace-scoped credentials."""

from lib.acceptance.clients.lifecycle import LifecycleClient
import hashlib
import json
from pathlib import Path
import secrets
import tempfile
import time
import uuid

from lib.platform.sandbox import Sessions
from lib.core.errors import SessionError


class APIAcceptance(LifecycleClient):


    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);runtime=sessions.attach();bundle=sessions.bundle(require_client=False);sessions.verify(bundle)
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
                sessions.create(sessions.render('session',params))
                sessions.create(sessions.render('api-rbac',params))
                sessions.create(sessions.render('api-quota',params))
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
