"""Offline image integrity, resource separation, and guarded product cleanup."""
from contextlib import nullcontext
import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch
import http.client
import ssl

import yaml
from PIL import Image

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.image_worker import outputs, transform, sha
from lib.platform_manifests import read_yaml, canonical, render, objects
from lib.platform_batch import BatchError
from lib.platform_batch_product import ProductAcceptance, cleanup_product, build_worker


class ImageBatchTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def image(self):
        path = self.directory / 'sample.png'
        image = Image.new('RGB', (2, 2), 'white')
        image.putpixel((0, 0), (0, 0, 0))
        image.save(path)
        return path, {'images': [{'name': path.name, 'bytes': path.stat().st_size,
            'sha256': sha(path.read_bytes()), 'width': 2, 'height': 2}]}

    def test_real_png_processing_preserves_known_pixels_and_never_upscales(self):
        _, manifest = self.image()
        values = dict(outputs(self.directory, manifest))
        self.assertEqual(set(values), {'sample-thumb.png', 'sample-gray.png'})
        for image in values.values():
            self.assertEqual(image.size, (2, 2))
            self.assertEqual(image.getpixel((0, 0)), (0, 0, 0))
            self.assertEqual(image.getpixel((1, 1)), (255, 255, 255))

    def test_input_corruption_dimensions_and_symlink_are_rejected(self):
        path, manifest = self.image()
        changed = copy.deepcopy(manifest)
        changed['images'][0]['sha256'] = '0' * 64
        with self.assertRaises(ValueError):
            outputs(self.directory, changed)
        changed = copy.deepcopy(manifest)
        changed['images'][0]['width'] = 999
        with self.assertRaises(ValueError):
            outputs(self.directory, changed)
        target = self.directory / 'elsewhere.png'
        path.rename(target)
        path.symlink_to(target)
        with self.assertRaises(ValueError):
            outputs(self.directory, manifest)

    def test_unknown_input_path_and_broken_decoder_data_are_rejected(self):
        path, manifest = self.image()
        manifest['images'][0]['name'] = '../sample.png'
        with self.assertRaises(ValueError):
            outputs(self.directory, manifest)
        path.write_bytes(b'not an image')
        item = {'name': path.name, 'bytes': path.stat().st_size, 'sha256': sha(path.read_bytes()), 'width': 2, 'height': 2}
        with self.assertRaises(OSError):
            outputs(self.directory, {'images': [item]})

    def test_frozen_dataset_contract_is_bound_to_the_reviewed_manifest(self):
        config = ROOT / 'deploy/agentbox/batch-images'
        manifest = read_yaml(config / 'dataset.yaml')
        contract = read_yaml(config / 'contract.yaml')
        self.assertEqual(contract['dataset_sha256'], canonical(manifest))
        self.assertEqual(len(contract['outputs']), 8)
        self.assertEqual({v['name'] for v in manifest['images']}, {'chelsea.png', 'coffee.png', 'astronaut.png', 'hubble_deep_field.jpg'})
        self.assertTrue(all(v['license'] in ('CC0-1.0', 'Public domain') and v['credit'] for v in manifest['images']))

    def test_api_pvc_rbac_and_worker_attempt_mounts_are_separate(self):
        params = {'OWNER': 'a' * 32, 'RUN': 'fresh', 'NAME': 'product-test', 'NAMESPACE': 'product-test',
            'QUEUE_NAME': 'queue-test', 'FLAVOR_NAME': 'flavor-test', 'API_IMAGE': 'agentbox/api:test',
            'WORKER_IMAGE': 'agentbox/image-worker:test', 'WORKER_IMAGE_ID': 'sha256:' + '1' * 64, 'WORKER_IMAGE_DIGEST': 'sha256:' + '2' * 64, 'API_INSTANCE': 'batch-test'}
        values = objects(render(ROOT / 'deploy', 'batch-api-resources', params))
        self.assertTrue(all(v['metadata'].get('namespace', 'product-test') == 'product-test' for v in values))
        config = next(v for v in values if v['kind'] == 'ConfigMap')
        job = yaml.safe_load(config['data']['job.yaml'])
        self.assertEqual(job['metadata']['namespace'], 'product-test')
        pod = job['spec']['template']['spec']
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertEqual(pod['containers'][0]['volumeMounts'][0]['subPathExpr'], 'attempts/$(JOB_UID)/$(POD_UID)')
        self.assertEqual(pod['initContainers'][0]['image'], params['WORKER_IMAGE'])
        self.assertEqual(pod['containers'][0]['image'], params['WORKER_IMAGE'])
        self.assertNotIn('ttlSecondsAfterFinished', job['spec'])
        self.assertEqual(job['spec']['backoffLimit'], 1)
        role = next(v for v in values if v['kind'] == 'Role')
        self.assertFalse(any('secrets' in r['resources'] for r in role['rules']))
        self.assertFalse(any('pods' in r['resources'] and ('create' in r['verbs'] or 'delete' in r['verbs']) for r in role['rules']))
        deployment = next(v for v in values if v['kind'] == 'Deployment')
        self.assertIn('--mode=batch', deployment['spec']['template']['spec']['containers'][0]['args'])
        self.assertIn('--namespace=product-test', deployment['spec']['template']['spec']['containers'][0]['args'])

    def test_product_cleanup_rejects_foreign_scope_before_cluster_access(self):
        path = self.directory / 'bad.json'
        path.write_text(json.dumps({'schema_version': 1, 'run': 'a' * 10, 'namespace': 'unrelated'}))
        lab = Mock()
        with self.assertRaises(BatchError):
            cleanup_product(lab, path)
        lab.owned.assert_not_called()
        lab.kubectl.assert_not_called()

    def test_product_cleanup_refuses_a_replaced_namespace(self):
        lab = Mock()
        lab.root, lab.config, lab.runner.output = ROOT, ROOT / 'deploy', self.directory
        fixture = ProductAcceptance(lab)
        run = 'b' * 10
        fixture.kueue.state = {'session': 'a' * 32}
        fixture.record = {'run': run, 'namespace': 'agentbox-batch-product-' + run, 'owner': 'a' * 32,
            'resources': [{'kind': 'Namespace', 'uid': 'original'}], 'jobs': {}}
        with patch.object(fixture, 'get', return_value={'metadata': {'name': fixture.record['namespace'], 'uid': 'replacement',
                'labels': {'agentbox.dev/session': 'a' * 32, 'agentbox.dev/batch-run': run}}}):
            with self.assertRaises(BatchError):
                fixture.cleanup()
        lab.kubectl.assert_not_called()

    def test_transport_recovery_reuses_the_exact_idempotent_submit_body(self):
        lab = Mock(); lab.root, lab.config, lab.runner.output = ROOT, ROOT / 'deploy', self.directory
        fixture = ProductAcceptance(lab)
        first, second = Mock(), Mock()
        first.sock = None; second.sock = None
        first.getresponse.side_effect = ssl.SSLEOFError('closed tunnel')
        second.getresponse.return_value.status = 200
        second.getresponse.return_value.read.return_value = b'{"batch_id":"same"}'
        fixture.http = first
        body = {'request_id': 'stable', 'dataset_id': 'image-small-v1'}
        with patch.object(fixture, 'connection', side_effect=[first, second]), patch('lib.platform_batch_product.time.sleep'):
            status, raw = fixture.request('POST', '/v2/batches', 'private', body)
        self.assertEqual(status, 200)
        self.assertEqual(first.request.call_args.args[2], second.request.call_args.args[2])
        self.assertNotIn('private', json.dumps(fixture.record))

    def test_transport_recovery_never_replays_session_execution(self):
        lab = Mock(); lab.root, lab.config, lab.runner.output = ROOT, ROOT / 'deploy', self.directory
        fixture = ProductAcceptance(lab)
        connection = Mock(); connection.sock = None
        connection.getresponse.side_effect = http.client.RemoteDisconnected('closed')
        fixture.http = connection
        with patch.object(fixture, 'connection', return_value=connection):
            with self.assertRaises(http.client.RemoteDisconnected):
                fixture.request('POST', '/v2/sessions/id/execute', 'private', {'code': 'must not replay'})
        connection.request.assert_called_once()

    def test_certificate_errors_are_not_treated_as_transient_transport(self):
        lab = Mock(); lab.root, lab.config, lab.runner.output = ROOT, ROOT / 'deploy', self.directory
        fixture = ProductAcceptance(lab)
        connection = Mock(); connection.sock = None
        connection.getresponse.side_effect = ssl.SSLCertVerificationError('wrong CA')
        fixture.http = connection
        with patch.object(fixture, 'connection', return_value=connection):
            with self.assertRaises(ssl.SSLCertVerificationError):
                fixture.request('GET', '/readyz', 'private')
        connection.request.assert_called_once()


if __name__ == '__main__':
    unittest.main()
