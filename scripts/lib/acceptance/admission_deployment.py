"""Controller Pod, scoped in-cluster credentials, real HTTPS queries and CNI faults."""
import copy
import ipaddress
import json
from pathlib import Path
import ssl
import subprocess
import tempfile
import time
import uuid
from http.server import ThreadingHTTPServer
import threading

from lib.acceptance.admission import AdmissionAcceptance, HealthFixture
from lib.acceptance.clients.batch_environment import BatchEnvironment
from lib.acceptance.clients.session import SessionClient
from lib.core.validation import require
from lib.manifests import read_yaml
from lib.manifests.network import discover_api_endpoints
from lib.observability.environment import Forward
from lib.platform.sandbox import Sessions
from lib.product.admission import compose
from lib.product.admission_image import build_admission_image
from lib.product.deployment import write_bundle
from lib.product.worker_image import worker_bundle


class AdmissionDeploymentAcceptance(AdmissionAcceptance):
    def test(self):
        with self.lab.operation_lock():
            sessions=Sessions(self.lab);runtime=sessions.attach();sessions.verify(sessions.bundle(require_client=False))
            api,product=SessionClient(self.lab),BatchEnvironment(self.lab)
            product.runtime,product.state=runtime,sessions.state;product.kueue.attach();product.kueue.verify()
            run=uuid.uuid4().hex[:10];owner=sessions.state['session'];controller_ns='agentbox-admission-'+run
            self.record.update(run=run,owner=owner,started_at=time.time(),scope='Actual controller Pod deployment and HTTPS/NetworkPolicy faults; no performance qualification')
            fixture=tls_server=tls_thread=forward=None
            ids=[];node=sessions.state['cluster']+'-control-plane'
            def node_command(*args):
                return self.lab.runner.run([self.lab.tools['docker'],'exec',node,*args],announce=False)
            try:
                self.stack.up();self.stack.reload_prometheus()
                with tempfile.TemporaryDirectory(prefix='admission-pod-private-',dir=self.lab.state_dir) as tmp:
                    private=Path(tmp);batch_private=private/'batch';batch_private.mkdir()
                    image=api.build_image(sessions);worker=worker_bundle(self.lab)
                    product.start(sessions,runtime,image,worker,batch_private,run)
                    prepared=build_admission_image(self.lab,sessions);self.record['image']=prepared;self.save()
                    gateway=self.lab.runner.run([self.lab.tools['docker'],'inspect',node,'--format',
                        '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}'],announce=False).strip()
                    address=ipaddress.ip_address(gateway)
                    require(address.is_private and not address.is_loopback,'unexpected_admission_bridge','private owned kind bridge required')
                    self.lab.runner.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=admission-query-fixture',
                        '-addext','subjectAltName=IP:'+gateway,'-keyout',private/'query.key','-out',private/'query.crt'],timeout=20,announce=False)
                    (private/'query.key').chmod(0o600)
                    fixture=HealthFixture(self.output/'injected-health.jsonl','pod-'+run);fixture.set('good')
                    tls_server=ThreadingHTTPServer((gateway,0),fixture.server.RequestHandlerClass);tls_server.daemon_threads=True
                    context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);context.minimum_version=ssl.TLSVersion.TLSv1_2
                    context.load_cert_chain(private/'query.crt',private/'query.key')
                    tls_server.socket=context.wrap_socket(tls_server.socket,server_side=True)
                    tls_thread=threading.Thread(target=tls_server.serve_forever,daemon=True);tls_thread.start()
                    endpoint={'address':gateway,'port':tls_server.server_port}
                    self.policy=read_yaml(self.lab.config/'agentbox/admission/policy.example.yaml')
                    self.policy.update(namespace=product.record['namespace'],cluster_queue=product.record['namespace'],owner=owner,
                        admission_check='agentbox-health-'+run,prometheus_url='https://'+gateway+':'+str(endpoint['port']),
                        ca_file='/etc/agentbox/prometheus-ca.pem',minimum_samples=2,max_age_seconds=10,stable_seconds=4,
                        protect_seconds=2,permit_interval_seconds=6,max_wait_seconds=120,retry_seconds=5,poll_seconds=1,request_timeout_seconds=3)
                    self.policy['health'].update(revision=fixture.revision,window_seconds=2,deadline_seconds=1)
                    resources=compose(self.lab.config,self.policy,prepared['reference'],controller_ns,
                                      discover_api_endpoints(self.lab.kubectl),[endpoint],(private/'query.crt').read_text())
                    deployment=next(o for o in resources if o['kind']=='Deployment')
                    pod_spec=deployment['spec']['template']['spec']
                    pod_spec['nodeSelector']={'kubernetes.io/hostname':node}
                    pod_spec['tolerations']=[{'key':'node-role.kubernetes.io/control-plane','operator':'Exists','effect':'NoSchedule'}]
                    write_bundle(self.output/'admission-pod-manifests.yaml',resources)
                    order={'Namespace':0,'ServiceAccount':1,'Role':2,'ClusterRole':2,'RoleBinding':3,'ClusterRoleBinding':3,
                           'AdmissionCheck':4,'ConfigMap':5,'NetworkPolicy':6,'Deployment':7}
                    for obj in sorted(resources,key=lambda o:order[o['kind']]):self.create(obj)
                    self.credentials(private,controller_ns)
                    self.stack.control_targets({'health':fixture.address})
                    self.lab.kubectl('-n',controller_ns,'rollout','status','deployment/agentbox-admission','--timeout=180s',timeout=190)
                    pods=json.loads(self.lab.kubectl('-n',controller_ns,'get','pods','-l','app=agentbox-admission','-o','json',announce=False))['items']
                    require(len(pods)==1,'admission_pod_count','one deployed controller required')
                    pod=pods[0]
                    require(pod['spec']['nodeName']==node and pod['spec']['serviceAccountName']=='agentbox-admission',
                            'admission_pod_identity_changed','controller placement/identity differs')
                    self.record['controller_pod']={'name':pod['metadata']['name'],'uid':pod['metadata']['uid'],'image':pod['status']['containerStatuses'][0]['imageID']}
                    forward=Forward(self.lab,controller_ns,pod['metadata']['name'],self.output/'admission-pod-metrics-forward.log')
                    self.stack.control_targets({'health':fixture.address,'admission-0':forward.address})
                    self.wait('up{job="agentbox-control",instance="admission-0"} == 1')
                    self.wait('agentbox_admission_sample_valid{instance="admission-0"} == 1')
                    self.mark('actual_pod_incluster_identity_and_verified_https_ca_query')
                    check=self.policy['admission_check']
                    attached=self.patch('clusterqueue',product.record['namespace'],'/spec/admissionChecksStrategy',{'admissionChecks':[{'name':check}]})
                    require(attached['spec']['admissionChecksStrategy']=={'admissionChecks':[{'name':check}]},'admission_attachment_pruned','check must be attached')
                    product.quota(1)  # Also wait for this CQ generation to become Active.
                    first=self.submit(product,'pod-https-positive')
                    result=product.wait_task(first,'succeeded');product.verify_artifacts(result)
                    require(self.check_state(product,first,'Ready'),'admission_ready_not_observed','real controller must have issued this permit')
                    product.api('DELETE','/v2/batches/'+first['batch_id'],product.alice,expected={200,202})
                    product.wait_task(first,'succeeded',cleanup=True)
                    self.mark('deployed_controller_admits_real_job_and_validates_eight_pngs')
                    policy=product.get('networkpolicy','agentbox-admission',controller_ns)
                    original=policy['spec']['egress']
                    restricted=[r for r in original if not any(peer.get('ipBlock',{}).get('cidr')==gateway+'/32' for peer in r.get('to',[]))]
                    require(len(restricted)<len(original),'missing_health_egress_rule','fault must remove exactly the health path')
                    try:
                        self.patch('networkpolicy','agentbox-admission','/spec/egress',restricted,controller_ns)
                        self.wait('agentbox_admission_state{state="degraded",instance="admission-0"} == 1',timeout=60)
                        held=self.submit(product,'pod-network-recovery');self.held(product,held)
                        self.mark('real_cni_query_block_stops_new_permissions')
                    finally:
                        current=product.get('networkpolicy','agentbox-admission',controller_ns)
                        require(current['metadata']['uid']==policy['metadata']['uid'] and current['spec']['egress']==restricted,
                                'admission_network_policy_changed','refusing to overwrite external policy changes')
                        self.patch('networkpolicy','agentbox-admission','/spec/egress',original,controller_ns)
                    result=product.wait_task(held,'succeeded');product.verify_artifacts(result)
                    product.api('DELETE','/v2/batches/'+held['batch_id'],product.alice,expected={200,202})
                    product.wait_task(held,'succeeded',cleanup=True)
                    self.mark('cni_restore_rebuilds_health_and_recovers_same_job')
                    self.record['all_passed']=True;self.save()
                    self.record['controller_logs']='controller-pod.jsonl'
                    (self.output/'controller-pod.jsonl').write_text(self.lab.kubectl('-n',controller_ns,'logs',pod['metadata']['name'],'--tail=10000',announce=False))
            finally:
                errors=[]
                if product.record.get('namespace'):
                    try:product.teardown()
                    except Exception as error:errors.append({'stage':'batch','reason':getattr(error,'reason',None)})
                try:
                    sandboxes=json.loads(node_command('crictl','pods','-o','json'))['items']
                    ids=[s['id'] for s in sandboxes if s.get('metadata',{}).get('namespace')==controller_ns]
                    if forward:forward.close()
                    self.archive_control_metrics();self.stack.control_targets({})
                except Exception as error:errors.append({'stage':'diagnostics','reason':getattr(error,'reason',None)})
                try:
                    self.cleanup_controller()
                    until=time.monotonic()+90
                    while True:
                        remaining=[s for s in json.loads(node_command('crictl','pods','-o','json'))['items'] if s['id'] in ids]
                        if not remaining:break
                        require(time.monotonic()<until,'admission_controller_cri_remaining','controller sandbox not reclaimed')
                        time.sleep(.5)
                    self.record['controller_cri_sandboxes_reclaimed']=ids
                except Exception as error:errors.append({'stage':'controller','reason':getattr(error,'reason',None)})
                if tls_server:tls_server.shutdown();tls_server.server_close()
                if tls_thread:tls_thread.join(timeout=5)
                if fixture:fixture.close()
                self.record['cleanup_complete']=not errors and product.record.get('cleanup_complete',True)
                self.record['admission_cleanup_errors']=errors
                self.record['all_passed']=self.record['all_passed'] and self.record['cleanup_complete'];self.save()
            return self.record
