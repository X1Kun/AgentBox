"""Exercise complete deployment bundles in disposable, UID-scoped namespaces."""
import base64
import json
from pathlib import Path
import secrets
import ssl
import subprocess
import sys
import tempfile
import uuid

from lib.api_network import discover_api_endpoints
from lib.platform_api_execution import APIExecutionAcceptance
from lib.platform_batch import require
from lib.platform_batch_product import worker_bundle, import_image
from lib.platform_sessions import Sessions
from lib.product_deployment import compose, credentials, validate_cluster, write_bundle
from lib.product_images import prepare_product_images
from lib.static_baseline import BaselineProduct


class ProductEntryAcceptance:
    def __init__(self, lab):
        self.lab, self.output = lab, lab.runner.output
        self.record = {'schema_version':1,'all_passed':False,'cleanup_complete':False,'namespaces':{}}

    def save(self):
        self.lab.write_json(self.output/'product-entry-test.json',self.record)

    def test(self):
        with self.lab.operation_lock():
            sessions = Sessions(self.lab)
            runtime = sessions.attach()
            bundle = sessions.bundle(require_client=False)
            self.record['platform_verification'] = sessions.verify(bundle)
            api, batch = APIExecutionAcceptance(self.lab), BaselineProduct(self.lab)
            batch.runtime, batch.state = runtime, sessions.state
            batch.kueue.attach(); batch.kueue.verify()
            run, owner = uuid.uuid4().hex[:10], sessions.state['session']
            self.record.update(run=run,owner=owner)
            api.record.update(run=run,owner=owner,runtime_evidence={})
            self.save()
            try:
                worker = worker_bundle(self.lab)
                prepared = prepare_product_images(self.lab,with_batch=True)
                api.record.update(prepared['api_build'])
                endpoints = discover_api_endpoints(self.lab.kubectl)
                token, other = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
                with tempfile.TemporaryDirectory(prefix='product-entry-private-',dir=self.lab.state_dir) as tmp:
                    directory=Path(tmp)
                    subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj',
                        '/CN=agentbox-entry','-addext','subjectAltName=IP:127.0.0.1','-keyout',directory/'tls.key',
                        '-out',directory/'tls.crt'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
                    (directory/'tls.key').chmod(0o600)
                    principals=directory/'principals.json'
                    principals.write_text(json.dumps([{'id':'alice','token':token},{'id':'bob','token':other}]))
                    principals.chmod(0o600)
                    private=credentials(directory)
                    common={'schema_version':1,'owner':owner,'instance':run,'api_server_endpoints':endpoints}
                    session_input={**common,'product':'session','namespaces':{'work':'agentbox-entry-work-'+run,'api':'agentbox-entry-api-'+run},
                        'images':{'api':prepared['api']['reference'],'runtime':prepared['runtime']['reference']}}
                    self.lab.write_json(self.output/'session-test-inputs.json',session_input)
                    self.record['session_prerequisites']=validate_cluster(self.lab.config,session_input,self.lab.kubeconfig,'kind-agentbox-platform-lab')
                    session_resources=compose(self.lab.config,session_input,private)
                    write_bundle(directory/'session.yaml',session_resources)
                    for obj in session_resources:
                        if obj['kind']=='Secret':
                            self.lab.kubectl('create','-f','-',input_text=json.dumps(obj));continue
                        created=json.loads(self.lab.kubectl('create','-f','-','-o','json',input_text=json.dumps(obj),announce=False))
                        if obj['kind']=='Namespace':
                            role='api' if obj['metadata']['name']==session_input['namespaces']['api'] else 'work'
                            self.record['namespaces'][role]={'name':created['metadata']['name'],'uid':created['metadata']['uid']};self.save()
                    api.tls_context=ssl.create_default_context(cafile=str(directory/'tls.crt'))
                    api.connect(session_input['namespaces']['api'])
                    view=api.session('entry-session',token,runtime,session_input['namespaces']['work'])
                    api.file(view,token,'entry-marker',b'entry-ready')
                    _,result=api.execute(view,token,'print(6*7)',expected={200})
                    require(result['stdout']=='42\n' and result['exit_code']==0,'entry_session_wrong','execution mismatch')
                    _,result=api.file(view,token,'entry-marker')
                    require(base64.b64decode(result['content_base64'])==b'entry-ready','entry_file_wrong','file mismatch')
                    api.close(view,token)
                    self.record['session']={'objects':len(session_resources),'execution_and_files':True,'view':view,
                        'public_resources':[o for o in session_resources if o['kind']!='Secret']};self.save()
                    batch_input={**common,'product':'batch','namespaces':{'work':'agentbox-batch-product-'+run},
                        'images':{'api':session_input['images']['api'],'worker':prepared['worker']['reference']},
                        'worker_config_id':worker['image_id'],'worker_manifest_digest':worker['image_digest']}
                    self.lab.write_json(self.output/'batch-test-inputs.json',batch_input)
                    self.record['batch_prerequisites']=validate_cluster(self.lab.config,batch_input,self.lab.kubeconfig,'kind-agentbox-platform-lab')
                    batch_resources=compose(self.lab.config,batch_input,private)
                    write_bundle(directory/'batch.yaml',batch_resources)
                    batch.record.update(owner=owner,run=run,namespace=batch_input['namespaces']['work'],resources=[],jobs={},views={},
                                        runtime_evidence={},worker=worker,cleanup_complete=False)
                    for obj in batch_resources:
                        if obj['kind']=='Secret':
                            self.lab.kubectl('create','-f','-',input_text=json.dumps(obj));continue
                        batch.create(obj)
                    batch.alice=token
                    batch.tls_context=api.tls_context
                    batch.connect(batch.record['namespace'])
                    _,view=batch.api('POST','/v2/batches',token,{'request_id':'entry-images','dataset_id':worker['dataset'],'passes':1},expected={202})
                    result=batch.wait_task(view,'succeeded')
                    batch.capture(view)
                    outputs=batch.verify_artifacts(result)
                    self.record['batch']={'objects':len(batch_resources),'artifacts':sorted(outputs),
                        'public_resources':[o for o in batch_resources if o['kind']!='Secret']}
                    self.record['all_passed']=True
            finally:
                primary=sys.exc_info()[1]
                if primary:self.record['primary_failure']={'type':type(primary).__name__,'reason':getattr(primary,'reason',None)}
                errors=[]
                def attempt(stage,action):
                    try:action()
                    except Exception as error:errors.append({'stage':stage,'type':type(error).__name__,'reason':getattr(error,'reason',None)})
                attempt('api_stop',api.stop)
                if batch.record.get('namespace'):attempt('batch_delete',batch.teardown)
                for role,ref in self.record['namespaces'].items():
                    attempt('session_delete_'+role,lambda ref=ref:sessions.close_namespaces({'scope':ref},run))
                for evidence in api.record['runtime_evidence'].values():
                    attempt('session_runtime',lambda evidence=evidence:require(not runtime.processes(evidence['sandbox_id']),'entry_runtime_remaining','runtime remains'))
                self.record.update(cleanup_errors=errors,cleanup_complete=not errors)
                if errors:self.record['all_passed']=False
                self.save()
                require(not errors,'product_entry_cleanup_incomplete','see product-entry-test.json')
            return self.record
