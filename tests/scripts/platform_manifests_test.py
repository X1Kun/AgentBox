"""Offline Kustomize semantics, identity bridges and parser safety."""
import copy
import json
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'scripts'))
from lib.platform_manifests import ManifestError,canonical,definition,lock_identity,normalized,objects,read_yaml,load_lock,LOCK_PATHS,render,render_component,profiles,python_image_source
import yaml


class ManifestTests(unittest.TestCase):
    def test_all_pre_migration_resource_shapes_are_equivalent(self):
        baseline=read_yaml(ROOT/'tests/fixtures/platform-manifests.yaml')
        for profile,expected in baseline['profiles'].items():
            with self.subTest(profile=profile):
                value=render(ROOT/'deploy',profile)
                self.assertEqual(canonical(normalized(value)),expected)
                self.assertFalse(any(o['kind']=='ConfigMap' and o['metadata']['name']=='agentbox-render-parameters' for o in objects(value)))

    def test_dynamic_parameter_injection_matches_original_resources(self):
        baseline=read_yaml(ROOT/'tests/fixtures/platform-manifests.yaml')
        for profile,expected in baseline['dynamic_profiles'].items():
            with self.subTest(profile=profile):
                self.assertEqual(canonical(normalized(render(ROOT/'deploy',profile,baseline['dynamic_parameters']))),expected)

    def test_rbac_namespace_refs_and_selectors_are_not_broadly_transformed(self):
        params={'OWNER':'a'*32,'RUN':'fresh-run','WORK_NS':'work-test','CLIENT_NS':'client-test','API_IMAGE':'registry:5000/project/api@sha256:'+'1'*64}
        value=render(ROOT/'deploy','api-rbac',params)
        for obj in objects(value):
            self.assertEqual(obj['metadata']['namespace'],'client-test' if obj['kind']=='ServiceAccount' else 'work-test')
            if obj['kind']=='RoleBinding':self.assertEqual(obj['subjects'][0]['namespace'],'client-test')
            if obj['kind']=='Role':
                self.assertFalse(any('delete' in r['verbs'] for r in obj['rules']))
        api=render(ROOT/'deploy','api',params)
        self.assertEqual(api['spec']['selector'],{'matchLabels':{'app':'agentbox-api'}})
        self.assertIn('--namespace=work-test',api['spec']['template']['spec']['containers'][0]['args'])
        self.assertIn('--instance=api-fresh-run',api['spec']['template']['spec']['containers'][0]['args'])
        self.assertEqual(api['spec']['template']['spec']['containers'][0]['image'],params['API_IMAGE'])
        policies=objects(render(ROOT/'deploy','network-policy',{'OWNER':'a'*32,'RUN':'fresh-run','NAMESPACE':'isolated-net'}))
        dns=next(o for o in policies if o['metadata']['name']=='allow-dns')
        self.assertEqual(dns['spec']['egress'][0]['to'][0]['namespaceSelector']['matchLabels']['kubernetes.io/metadata.name'],'kube-system')
        self.assertTrue(all(o['metadata']['namespace']=='isolated-net' for o in policies))

    def test_every_orchestration_selection_and_dynamic_namespace_build(self):
        config=ROOT/'deploy'
        for name in profiles(config):
            with self.subTest(name=name):self.assertTrue(objects(render(config,name)))
        for name in ('platform-namespace','session-namespace','network-namespace'):
            value=render(config,name,{'OWNER':'a'*32,'RUN':'fresh','ROLE':'work','NAME':'isolated-case'})
            self.assertEqual(value['metadata']['name'],'isolated-case')
            self.assertEqual(value['metadata']['labels']['agentbox.dev/session'],'a'*32)

    def test_warm_and_cold_builds_do_not_share_mutable_state(self):
        warm=render(ROOT/'deploy','session-warm')
        warm['items'][1]['spec']['replicas']=99
        cold=render(ROOT/'deploy','session')
        self.assertEqual(cold['items'][1]['spec']['replicas'],0)
        self.assertEqual(render(ROOT/'deploy','session-warm')['items'][1]['spec']['replicas'],2)

    def test_lock_format_bridge_accepts_only_equal_values(self):
        config=ROOT/'deploy'
        baseline=read_yaml(config/'lab/format-compatibility.yaml')
        for name,entry in baseline['locks'].items():
            self.assertEqual(canonical(load_lock(config,name)),entry['semantic_sha256'])
            self.assertEqual(lock_identity(config,name),entry['legacy_sha256'])
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);shutil.copytree(config,root/'config');root=root/'config'
            value=read_yaml(root/'lab/versions.lock.yaml');value['kubectl']['sha256']='0'*64
            (root/'lab/versions.lock.yaml').write_text(yaml.safe_dump(value,sort_keys=False))
            self.assertNotEqual(lock_identity(root,'versions'),baseline['locks']['versions']['legacy_sha256'])

    def test_python_repository_override_always_uses_locked_digest(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = Path(tmp) / 'deploy'
            shutil.copytree(ROOT / 'deploy', config)
            original_identity = lock_identity(config, 'versions')
            source = config / 'lab/image-sources.yaml'
            source.write_text('schema_version: 1\npython_repository: mirror.example/python\n')
            image = load_lock(config, 'versions')['smoke']['image']
            self.assertEqual(python_image_source(config), 'mirror.example/python@' + image.split('@')[1])
            self.assertEqual(lock_identity(config, 'versions'), original_identity)
            lock = load_lock(config, 'versions')
            lock['smoke']['image'] = 'docker.io/library/python@sha256:' + '1' * 64
            (config / LOCK_PATHS['versions']).write_text(yaml.safe_dump(lock))
            self.assertEqual(python_image_source(config), 'mirror.example/python@sha256:' + '1' * 64)
            self.assertNotEqual(lock_identity(config, 'versions'), original_identity)
            for repository in ('https://mirror.example/python', 'mirror.example/python:latest',
                               'mirror.example/python@sha256:bad', 'mirror.example/../python'):
                source.write_text(yaml.safe_dump({'schema_version': 1, 'python_repository': repository}))
                with self.subTest(repository=repository), self.assertRaises(ManifestError):
                    python_image_source(config)

    def test_logical_definition_bridge_rejects_security_or_placement_changes(self):
        config=ROOT/'deploy'
        bridge=read_yaml(config/'lab/format-compatibility.yaml')
        for name,value in bridge['definitions'].items():self.assertEqual(definition(config,name),value['legacy'])
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)/'config';shutil.copytree(config,root)
            p=root/'components/agent-sandbox/template.yaml'
            value=read_yaml(p)
            value['spec']['podTemplate']['spec']['automountServiceAccountToken']=True
            p.write_text(yaml.safe_dump(value,sort_keys=False))
            self.assertNotEqual(definition(root,'session'),bridge['definitions']['session']['legacy'])
            p=root/'lab/kind-calico.yaml';p.write_text(p.read_text()+'# changed\n')
            self.assertNotEqual(definition(root,'network'),bridge['definitions']['network']['legacy'])

    def test_component_layout_and_actual_environment_overlay(self):
        config=ROOT/'deploy'
        overlays=[p.relative_to(config).as_posix() for p in config.rglob('kustomization.yaml') if 'overlays' in p.parts]
        self.assertEqual(overlays,['agentbox/overlays/lab/kustomization.yaml'])
        self.assertFalse((config/'platform').exists())
        base=render_component(config,'api',environment='base')
        lab=render_component(config,'api',environment='lab')
        before=next(o for o in objects(base) if o['kind']=='Deployment')
        after=next(o for o in objects(lab) if o['kind']=='Deployment')
        self.assertNotIn('nodeSelector',before['spec']['template']['spec'])
        self.assertIn('nodeSelector',after['spec']['template']['spec'])
        self.assertEqual(before['spec']['selector'],after['spec']['selector'])

    def test_duplicate_keys_and_executable_yaml_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'bad.yaml'
            for text in ('version: one\nversion: two\n','!!python/object/apply:os.system ["must-not-execute"]'):
                path.write_text(text)
                with patch('os.system',side_effect=AssertionError('executed')):
                    with self.assertRaises(ManifestError):read_yaml(path)

    def test_remote_paths_invalid_parameters_and_missing_parameters_fail_before_build(self):
        with patch('lib.platform_manifests._build',side_effect=AssertionError('built')):
            with self.assertRaises(ManifestError):render(ROOT/'deploy','api',{'OWNER':'owner'})
            with self.assertRaises(ManifestError):render(ROOT/'deploy','api',{'WORK_NS':'../../escape'},strict=False)
            with self.assertRaises(ManifestError):render(ROOT/'deploy','api',{'TOKEN':'secret'},strict=False)
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)/'config';shutil.copytree(ROOT/'deploy',root)
            path=root/'agentbox/api/kustomization.yaml'
            config=read_yaml(path);config['resources']=['https://example.invalid/remote.yaml'];path.write_text(yaml.safe_dump(config,sort_keys=False))
            with patch('lib.platform_manifests._build',side_effect=AssertionError('built')):
                with self.assertRaises(ManifestError):render(root,'api')

if __name__=='__main__':unittest.main()
