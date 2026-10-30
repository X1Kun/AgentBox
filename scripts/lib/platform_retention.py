"""Metadata-only real Claim quota/retention acceptance; never starts a sandbox."""
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

from lib.platform_api import APIAcceptance
from lib.platform_sessions import Sessions, OWNER, RUN

CLAIMS = 'sandboxclaims.extensions.agents.x-k8s.io'
QUOTA_KEY = 'count/' + CLAIMS
CLOSED = 'agentbox.dev/api-closed-at'


class APIRetentionAcceptance(APIAcceptance):
    def save(self):
        (self.output/'api-retention-test.json').write_text(json.dumps(self.record, indent=2, sort_keys=True)+'\n')

    def wait(self, predicate, detail, seconds=120):
        until = time.monotonic()+seconds
        while time.monotonic() < until:
            value = predicate()
            if value:
                return value
            time.sleep(.5)
        self.require(False, detail)

    def quota(self, work, used):
        def ready():
            q = json.loads(self.lab.kubectl('-n',work,'get','resourcequota','agentbox-api-budget','-o','json',announce=False))
            return q if q.get('status',{}).get('used',{}).get(QUOTA_KEY)==str(used) else None
        return self.wait(ready, 'record quota did not converge')

    def claims(self, work):
        return json.loads(self.lab.kubectl('-n',work,'get',CLAIMS,'-o','json',announce=False))['items']

    def credentials(self, directory, work, client):
        prefix=[self.lab.tools['kubectl'],'--kubeconfig',str(self.lab.kubeconfig),'--context','kind-agentbox-platform-lab','--request-timeout=15s']
        # Token output is never routed through Runner or saved in evidence.
        token=subprocess.run(prefix+['-n',client,'create','token','agentbox-cleanup','--duration=3600s'],capture_output=True,text=True,check=True,timeout=30).stdout.strip()
        config=json.loads(subprocess.run(prefix+['config','view','--raw','--minify','-o','json'],capture_output=True,text=True,check=True,timeout=30).stdout)
        path=directory/'cleanup-kubeconfig'
        self.lab.write_json(path,{'apiVersion':'v1','kind':'Config','clusters':[{'name':'lab','cluster':config['clusters'][0]['cluster']}],
            'users':[{'name':'cleanup','user':{'token':token}}],'contexts':[{'name':'cleanup','context':{'cluster':'lab','user':'cleanup','namespace':work}}],'current-context':'cleanup'})
        checks=[('get','secrets',False),('create','pods',False),('delete','pods',False),('create',CLAIMS,False),('list',CLAIMS,True),('patch',CLAIMS,True),('delete',CLAIMS,True)]
        for verb,resource,allowed in checks:
            result=subprocess.run([self.lab.tools['kubectl'],'--kubeconfig',str(path),'--request-timeout=10s','-n',work,'auth','can-i',verb,resource],capture_output=True,text=True,timeout=15)
            self.require(result.stdout.strip()==('yes' if allowed else 'no'),'cleanup RBAC mismatch')
        for namespace,account,verb in [(work,'agentbox-api','delete'),(client,'agentbox-cleanup','delete')]:
            result=subprocess.run(prefix+['-n',namespace,'auth','can-i',verb,CLAIMS,'--as=system:serviceaccount:'+client+':'+account],capture_output=True,text=True,timeout=15)
            self.require(result.stdout.strip()=='no','API role or cross-namespace cleanup permission expanded')
        return path

    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);sessions.attach();bundle=sessions.bundle();sessions.verify(bundle)
            run=uuid.uuid4().hex[:10];namespaces={}
            self.record.update(run=run,owner=sessions.state['session'],namespaces=namespaces,
                scope='32 already-expired Retain Claims; controller acknowledgement is real; no executor Pods',
                io_pressure=Path('/proc/pressure/io').read_text())
            binary=self.lab.state_dir/'api-build'/'agentbox-cleanup';binary.parent.mkdir(exist_ok=True)
            self.lab.runner.run(['env','CGO_ENABLED=0','go','build','-p=2','-mod=readonly','-trimpath','-buildvcs=false','-o',binary,'./cmd/agentbox-cleanup'],timeout=900)
            self.record['binary_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
            try:
                for role in ('work','client'):
                    namespaces[role]=sessions.namespace('agentbox-retention-'+role+'-'+run,'retention-'+role,run)
                work=namespaces['work']['name'];client=namespaces['client']['name'];instance='retention-'+run
                params={'OWNER':sessions.state['session'],'RUN':run,'WORK_NS':work,'CLIENT_NS':client,'RUNTIME_IMAGE':bundle['images']['runtime']['name']}
                for name in ('session-template.json','api-rbac.json','cleanup-rbac.json','api-quota.json'):
                    sessions.create(sessions.render(name,params))
                self.quota(work,0)
                pool=json.loads(self.lab.kubectl('-n',work,'get','sandboxwarmpool','python-cold','-o','json'))
                owner=hashlib.sha256(b'alice').hexdigest()[:32]
                digest=hashlib.sha256(json.dumps({'Pool':'python-cold','TTL':60},separators=(',',':')).encode()).hexdigest()
                deadline=(datetime.now(timezone.utc)-timedelta(minutes=10)).strftime('%Y-%m-%dT%H:%M:%SZ')

                def fixture(index):
                    name='ses-'+hashlib.sha256((instance+'\0alice\0record-'+str(index)).encode()).hexdigest()[:40]
                    return {'apiVersion':'extensions.agents.x-k8s.io/v1beta1','kind':'SandboxClaim','metadata':{'name':name,'namespace':work,
                        'labels':{'agentbox.dev/api-owner':owner,'agentbox.dev/api-instance':instance,'app.kubernetes.io/managed-by':'agentbox-api'},
                        'annotations':{'agentbox.dev/api-request-digest':digest,'agentbox.dev/api-ttl-seconds':'60','agentbox.dev/api-pool-uid':pool['metadata']['uid']}},
                        'spec':{'warmPoolRef':{'name':'python-cold'},'lifecycle':{'shutdownTime':deadline,'shutdownPolicy':'Retain'}}}

                for i in range(32):
                    sessions.create(fixture(i))
                self.quota(work,32)
                prefix=[self.lab.tools['kubectl'],'--kubeconfig',str(self.lab.kubeconfig),'--context','kind-agentbox-platform-lab','--request-timeout=15s']
                rejected=subprocess.run(prefix+['create','-f','-'],input=json.dumps(fixture(32)),capture_output=True,text=True,timeout=30)
                self.require(rejected.returncode!=0 and 'Forbidden' in rejected.stderr and 'exceeded quota' in rejected.stderr,'33rd record did not fail specifically on quota')
                self.mark('record_quota_exhaustion_rejects_33rd_claim')

                def expired():
                    items=self.claims(work)
                    return items if len(items)==32 and all(any(c.get('type')=='Ready' and c.get('status')=='False' and c.get('reason')=='ClaimExpired' and c.get('observedGeneration')==v['metadata']['generation'] for c in v.get('status',{}).get('conditions',[])) for v in items) else None
                initial=self.wait(expired,'native controller did not acknowledge expired records')
                original={v['metadata']['name']:v['metadata']['uid'] for v in initial}
                self.record['original_claim_uids']=original
                with tempfile.TemporaryDirectory(prefix='retention-private-',dir=self.lab.state_dir) as tmp:
                    credentials=self.credentials(Path(tmp),work,client)
                    self.mark('independent_cleanup_role_and_unchanged_product_role')
                    def sweep(label, retention, apply=False):
                        report=self.output/(label+'.json')
                        args=[binary,'--kubeconfig',credentials,'--namespace',work,'--pool','python-cold','--instance',instance,'--principal','alice','--retention',retention,'--report',report]
                        if apply:args+=['--apply']
                        self.lab.runner.run(args,timeout=150)
                        value=json.loads(report.read_text())
                        self.require(not value.get('error') and not value['truncated'],'cleanup report incomplete')
                        self.record.setdefault('sweeps',[]).append({'report':str(report),'sha256':hashlib.sha256(report.read_bytes()).hexdigest(),'outcomes':[v['outcome'] for v in value['entries']]})
                        return value
                    preview=sweep('preview-legacy','1h')
                    self.require(len(preview['entries'])==32 and all(v['outcome']=='would_finalize' for v in preview['entries']),'legacy preview skipped closure validation')
                    self.require(preview['quota_before']['pressure']=='exhausted','quota exhaustion not reported')
                    self.require(all(CLOSED not in v['metadata'].get('annotations',{}) for v in self.claims(work)),'preview mutated closure timestamps')
                    self.mark('default_preview_is_read_only_and_reports_quota_pressure')
                    finalized=sweep('apply-finalize','1h',True)
                    self.require(len(finalized['entries'])==32 and all(v['outcome']=='finalized' for v in finalized['entries']),'legacy records not finalized')
                    retained=sweep('preview-retained','1h')
                    self.require(all(v['outcome']=='retain' for v in retained['entries']),'legacy retention was backdated')
                    self.mark('legacy_records_start_new_retention_window')
                    # A deliberately short administrator policy is confined to
                    # this metadata fixture; production default remains 24h.
                    time.sleep(1.1)
                    removed=sweep('apply-delete','1s',True)
                    self.require(len(removed['entries'])==32 and all(v['outcome'] in ('deleted','deleting','original_absent') for v in removed['entries']),'aged records not deleted')
                self.wait(lambda:len(self.claims(work))==0,'deleted records remain')
                self.quota(work,0)
                sessions.create(fixture(0))
                current=json.loads(self.lab.kubectl('-n',work,'get',CLAIMS,fixture(0)['metadata']['name'],'-o','json'))
                self.require(current['metadata']['uid']!=original[current['metadata']['name']],'recreated record reused old UID')
                self.record['recreated_claim']={'name':current['metadata']['name'],'uid':current['metadata']['uid']}
                self.mark('quota_recovers_and_same_name_gets_new_claim_uid')
                for resource in ('pods','sandboxes.agents.x-k8s.io'):
                    items=json.loads(self.lab.kubectl('-n',work,'get',resource,'-o','json'))['items']
                    self.require(not items,'metadata fixture unexpectedly created execution resources')
                sessions.close_namespaces(namespaces,run)
                self.record.update(cleanup_complete=True,all_passed=True,development_cluster_retained=True)
                return self.record
            finally:
                self.save()
