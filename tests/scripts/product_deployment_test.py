"""Complete bundle boundaries, explicit inputs and private rendering."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'scripts'))
from lib.product.deployment import compose, credentials, validate_inputs, validate_cluster, write_bundle
from lib.manifests import ManifestError
from lib.manifests.network import endpoint_rules
from lib.product.images import bind_digest
from lib.core.errors import ToolError
from lib.platform.sandbox import Sessions, RUNTIME_BUILD_INPUTS
import hashlib
import shutil
from unittest.mock import Mock
import yaml


class ProductDeployment(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.directory=Path(self.temp.name)
        self.value={'schema_version':1,'product':'session','owner':'a'*32,'instance':'product-test',
            'namespaces':{'work':'work-test','api':'api-test'},
            'images':{'api':'example/api@sha256:'+'1'*64,'runtime':'example/runtime@sha256:'+'2'*64},
            'api_server_endpoints':[{'address':'10.96.0.1','port':443},{'address':'172.18.0.2','port':6443}]}
        self.private={'tls.crt':b'certificate','tls.key':b'key','principals.json':b'[]'}

    def test_session_bundle_is_complete_and_contains_no_batch_or_cleanup_resources(self):
        values=compose(ROOT/'deploy',self.value,self.private)
        self.assertEqual(len(values),12)
        self.assertEqual({v['metadata']['name'] for v in values if v['kind']=='Namespace'},{'work-test','api-test'})
        self.assertFalse(any(v['kind'] in ('Job','ClusterQueue','ResourceFlavor') or v['metadata']['name']=='agentbox-cleanup' for v in values))
        secret=next(v for v in values if v['kind']=='Secret');self.assertEqual(secret['metadata']['namespace'],'api-test')
        policy=next(v for v in values if v['kind']=='NetworkPolicy')
        self.assertEqual(policy['spec']['egress'][-2:],endpoint_rules(self.value['api_server_endpoints']))
        deployment=next(v for v in values if v['kind']=='Deployment')
        self.assertIn('--namespace=work-test',deployment['spec']['template']['spec']['containers'][0]['args'])

    def batch(self):
        value=copy.deepcopy(self.value);value.update(product='batch',namespaces={'work':'batch-test'},
            images={'api':self.value['images']['api'],'worker':'example/worker@sha256:'+'3'*64},
            worker_config_id='sha256:'+'4'*64,worker_manifest_digest='sha256:'+'3'*64)
        return value

    def test_batch_uses_only_product_budget_and_bound_worker_identity(self):
        values=compose(ROOT/'deploy',self.batch(),self.private)
        self.assertEqual(len(values),15)
        budgets=[v for v in values if v['kind']=='ResourceQuota']
        self.assertEqual(len(budgets),1);self.assertEqual(budgets[0]['metadata']['name'],'batch-product-budget')
        self.assertEqual(budgets[0]['spec']['hard']['requests.cpu'],'600m')
        deployment=next(v for v in values if v['kind']=='Deployment')
        self.assertIn('--batch-image-id=sha256:'+'4'*64,deployment['spec']['template']['spec']['containers'][0]['args'])
        cm=next(v for v in values if v['kind']=='ConfigMap')
        job=yaml.safe_load(cm['data']['job.yaml'])
        self.assertEqual(job['metadata']['namespace'],'batch-test')
        self.assertEqual(job['spec']['template']['spec']['containers'][0]['image'],self.batch()['images']['worker'])

    def test_invalid_or_placeholder_inputs_fail_before_kustomize(self):
        changes=[{'images':{'api':'example/api:latest','runtime':self.value['images']['runtime']}},
                 {'namespaces':{'work':'same','api':'same'}},{'api_server_endpoints':[]},
                 {'api_server_endpoints':[{'address':'0.0.0.0/0','port':443}]}]
        with patch('lib.product.deployment.render_product_template',side_effect=AssertionError('built')):
            for change in changes:
                with self.subTest(change=change),self.assertRaises(ManifestError):compose(ROOT/'deploy',{**self.value,**change},self.private)
            value=self.batch();value['worker_manifest_digest']='sha256:'+'5'*64
            with self.assertRaises(ManifestError):compose(ROOT/'deploy',value,self.private)

    def test_private_output_is_exclusive_and_never_overwrites_existing_material(self):
        path=self.directory/'bundle.yaml'
        write_bundle(path,[{'apiVersion':'v1','kind':'Secret','data':{'private':'private-content'}}])
        self.assertEqual(path.stat().st_mode & 0o777,0o600)
        before=path.read_bytes()
        with self.assertRaises(FileExistsError):write_bundle(path,[])
        self.assertEqual(path.read_bytes(),before)

    def test_foreign_namespace_is_rejected_using_only_explicit_context(self):
        with patch('lib.product.deployment.subprocess.run',return_value=SimpleNamespace(returncode=0,stdout=json.dumps({'metadata':{'labels':{}}}))) as command:
            with self.assertRaises(ManifestError):validate_cluster(ROOT/'deploy',self.value,'/explicit/config','explicit-context')
            self.assertEqual(command.call_count,1)
            self.assertIn('/explicit/config',command.call_args.args[0]);self.assertIn('explicit-context',command.call_args.args[0])

    def test_cli_validates_credentials_and_keeps_secret_material_out_of_stdout(self):
        subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=entry-test',
            '-keyout',self.directory/'tls.key','-out',self.directory/'tls.crt'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=10)
        (self.directory/'tls.key').chmod(0o600)
        token='private-token-'+'x'*32
        p=self.directory/'principals.json';p.write_text(json.dumps([{'id':'alice','token':token}]));p.chmod(0o600)
        config=self.directory/'inputs.yaml';config.write_text(yaml.safe_dump(self.value))
        result=subprocess.run([sys.executable,ROOT/'scripts/render_product.py','--config',config,'--credentials-dir',self.directory,
            '--output',self.directory/'product.yaml'],text=True,capture_output=True,timeout=30,env=dict(os.environ,KUBECONFIG='/must/not/be/used'))
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(json.loads(result.stdout)['objects'],12)
        self.assertNotIn(token,result.stdout+result.stderr)
        p.chmod(0o644)
        with self.assertRaises(ManifestError):credentials(self.directory)

    def test_runtime_inputs_do_not_read_acceptance_files(self):
        for name in ('go.mod','go.sum'):shutil.copyfile(ROOT/name,self.directory/name)
        lab=Mock();lab.root=self.directory;lab.config=ROOT/'deploy';lab.state_dir=self.directory
        session=Sessions(lab)
        with patch('lib.manifests.config.FIXTURES',self.directory/'missing-acceptance'):
            self.assertEqual(set(session.build_inputs(runtime_only=True)),set(RUNTIME_BUILD_INPUTS))

    def test_runtime_bundle_does_not_require_cached_acceptance_image(self):
        lab=Mock();lab.root=self.directory;lab.config=ROOT/'deploy';lab.state_dir=self.directory
        lab.docker_info.return_value={'ID':'owned'}
        session=Sessions(lab)
        inputs={key:'fixed' for key in RUNTIME_BUILD_INPUTS}
        bundle={'inputs':inputs,'docker_id':'owned','images':{
            'controller':{'name':'controller','id':'controller-id'},'runtime':{'name':'runtime','id':'runtime-id'},
            'client':{'name':'deleted-client','id':'old-client-id'}}}
        session.bundle_path.write_text(json.dumps(bundle))
        session.build_inputs=Mock(return_value=inputs)
        def inspect(name):
            self.assertNotEqual(name,'deleted-client')
            return {'Id':name+'-id'}
        session.docker_image=Mock(side_effect=inspect)
        self.assertEqual(session.bundle(require_client=False),bundle)

    def test_digest_binding_verifies_content_before_adding_cache_alias(self):
        source='docker.io/agentbox/runtime:content'
        config_id='sha256:'+'5'*64
        raw=json.dumps({'schemaVersion':2,'config':{'digest':config_id}})
        sha='sha256:'+hashlib.sha256(raw.encode()).hexdigest()
        reference='docker.io/agentbox/runtime@'+sha
        lab=Mock();lab.tools={'docker':'docker'}
        tagged=[]
        def command(args,**kwargs):
            if 'content' in args:return raw
            if 'ls' in args:
                ref=args[-1].partition('name==')[2]
                return 'REF TYPE DIGEST\n'+(ref+' manifest '+sha+'\n' if ref==source or tagged else '')
            if 'tag' in args:tagged.append(args[-1]);return ''
            if 'label' in args:return ''
            if 'crictl' in args:return json.dumps({'images':[{'id':config_id,'repoDigests':[reference]}]})
            raise AssertionError(args)
        lab.runner.run.side_effect=command
        result=bind_digest(lab,'owned-node','agentbox/runtime:content',config_id)
        self.assertEqual(result['reference'],reference)
        self.assertEqual(tagged,[reference])
        tagged.clear()
        with self.assertRaises(ToolError):bind_digest(lab,'owned-node','agentbox/runtime:content','sha256:'+'6'*64)
        self.assertEqual(tagged,[])


if __name__=='__main__':unittest.main()
