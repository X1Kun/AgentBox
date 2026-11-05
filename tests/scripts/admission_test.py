"""Admission deployment isolation and fault-fixture protocol contracts."""
import copy
import json
from pathlib import Path
import sys
import unittest
import urllib.error
import urllib.request

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'scripts'))
from lib.manifests import ManifestError, read_yaml
from lib.product.admission import compose
from lib.acceptance.admission import AdmissionAcceptance, HealthFixture


class AdmissionTests(unittest.TestCase):
    def setUp(self):
        self.policy=read_yaml(ROOT/'deploy/agentbox/admission/policy.example.yaml')
        self.policy['prometheus_url']='https://prometheus.example.test:9090'
        self.policy.update(namespace='test-batch',cluster_queue='test-queue',admission_check='test-check',owner='test-owner')

    def render(self,policy=None,image=None):
        return compose(ROOT/'deploy',policy or self.policy,image or 'registry.example.test/admission@sha256:'+'a'*64,
                       'test-controller',[{'address':'10.0.0.1','port':443}],[{'address':'10.0.0.2','port':9090}])

    def test_bundle_is_opt_in_scoped_and_has_no_compute_or_queue_mutations(self):
        resources=self.render()
        self.assertFalse({'Job','ClusterQueue','LocalQueue','SandboxClaim','Secret'} & {o['kind'] for o in resources})
        role=next(o for o in resources if o['kind']=='Role' and o['metadata']['namespace']=='test-batch')
        self.assertEqual({v for r in role['rules'] for v in r['resources']},{'workloads','workloads/status','localqueues'})
        cluster=next(o for o in resources if o['kind']=='ClusterRole')
        self.assertEqual(cluster['metadata']['name'],'test-controller')
        names={r['resources'][0]:r['resourceNames'] for r in cluster['rules']}
        self.assertEqual(names['namespaces'],['test-batch'])
        self.assertEqual(names['clusterqueues'],['test-queue'])
        self.assertEqual(names['admissionchecks/status'],['test-check'])
        deployment=next(o for o in resources if o['kind']=='Deployment')
        self.assertIn('--lease-namespace=test-controller',deployment['spec']['template']['spec']['containers'][0]['args'])
        policy=next(o for o in resources if o['kind']=='NetworkPolicy')
        self.assertEqual([r['to'][0]['ipBlock']['cidr'] for r in policy['spec']['egress'] if 'ipBlock' in r['to'][0]],['10.0.0.1/32','10.0.0.2/32'])

    def test_placeholder_images_loopback_urls_and_foreign_scope_fail_before_render(self):
        for url in ('http://127.0.0.1:19090','https://127.0.0.1:19090','https://prometheus.example.invalid:9090','https://user:secret@example.test'):
            policy=copy.deepcopy(self.policy);policy['prometheus_url']=url
            with self.assertRaises(ManifestError):self.render(policy)
        with self.assertRaises(ManifestError):self.render(image='agentbox/admission:latest')
        self.policy['namespace']='../foreign'
        with self.assertRaises(ManifestError):self.render()

    def test_custom_ca_cannot_be_silently_omitted_from_a_complete_bundle(self):
        self.policy['ca_file']='/etc/agentbox/prometheus-ca.pem'
        with self.assertRaises(ManifestError):self.render()
        with self.assertRaises(ManifestError):
            compose(ROOT/'deploy',self.policy,'registry.example.test/admission@sha256:'+'a'*64,'test-controller',
                    [{'address':'10.0.0.1','port':443}],[{'address':'10.0.0.2','port':9090}],'not a certificate')

    def test_fault_source_injects_only_metrics_and_never_synthetic_prometheus_success(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            fixture=HealthFixture(Path(tmp)/'samples.jsonl','test-revision')
            try:
                opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with opener.open('http://'+fixture.address+'/metrics') as response:
                    self.assertEqual(response.headers.get_content_type(),'text/plain')
                    bad=response.read().decode()
                self.assertIn('field="timely_correct"} 0',bad)
                fixture.set('good')
                with opener.open('http://'+fixture.address+'/metrics') as response:good=response.read().decode()
                self.assertIn('field="timely_correct"} 100',good)
                fixture.set('query-denied')
                with self.assertRaises(urllib.error.HTTPError) as error:opener.open('http://'+fixture.address+'/api/v1/query?query=up')
                self.assertEqual(error.exception.code,403)
            finally:fixture.close()

    def test_pending_before_quota_reservation_is_not_a_reserved_wait_observation(self):
        fixture=object.__new__(AdmissionAcceptance)
        fixture.policy={'admission_check':'health'};fixture.record={};fixture.save=lambda:None
        workload={'metadata':{'ownerReferences':[{'uid':'job-uid'}]},'status':{'admissionChecks':[{'name':'health','state':'Pending'}]}}
        fixture.workloads=lambda product:[workload]
        self.assertIsNone(fixture.check_state(None,{'batch_id':'job.job-uid'},'Pending'))
        workload['status']['conditions']=[{'type':'QuotaReserved','status':'True'}]
        self.assertEqual(fixture.check_state(None,{'batch_id':'job.job-uid'},'Pending'),workload)


if __name__=='__main__':unittest.main()
