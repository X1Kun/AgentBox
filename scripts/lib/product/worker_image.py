"""Fixed image worker builds and the real batch product acceptance fixture."""

import hashlib
import json
from pathlib import Path
import re
import shutil
import tempfile
import time


from lib.core.errors import BatchError
from lib.core.validation import require
from lib.manifests import canonical, read_yaml
from lib.product.images import bind_digest
from lib.product.dataset import prepare


def docker_image(lab, name):
    return json.loads(lab.runner.run([lab.tools['docker'], 'image', 'inspect', name], announce=False))[0]


def import_image(lab, image, image_id, node, archive):
    def cached():
        values = json.loads(lab.runner.run([lab.tools['docker'], 'exec', node, 'crictl', 'images', '-o', 'json'], announce=False))['images']
        return any(v['id'] == image_id and 'docker.io/' + image in v.get('repoTags', []) for v in values)
    if cached():
        return True
    processes = lab.runner.run([lab.tools['docker'], 'exec', node, 'ps', '-eo', 'comm='], announce=False)
    require('ctr' not in processes.split(), 'batch_image_import_busy', 'an existing node image import must finish first')
    lab.runner.run([lab.tools['docker'], 'save', '-o', archive, image], timeout=300)
    lab.runner.run([lab.tools['kind'], 'load', 'image-archive', archive, '--name', 'agentbox-platform-lab', '--nodes', node], timeout=300)
    until = time.monotonic() + 60
    while time.monotonic() < until:
        if cached():
            return False
        time.sleep(.5)
    raise BatchError('batch_image_not_imported', 'node did not confirm the expected image ID and tag')


def build_worker(lab):
    cache, contract = prepare()
    with lab.operation_lock():
        state, _ = lab.owned(complete=True)
        base = lab.python_image()
        lab.prepare_image(base)
        original = docker_image(lab, base)
        require(base in original.get('RepoDigests', []), 'batch_base_not_confirmed', 'base digest not confirmed')
        files = ['deploy/agentbox/batch-images/dataset.yaml', 'deploy/agentbox/batch-images/versions.lock.yaml',
                 'deploy/agentbox/batch-images/contract.yaml', 'deploy/agentbox/batch-images/worker.Dockerfile',
                 'workloads/images/worker.py', 'scripts/lib/product/image_pipeline.py']
        inputs = {name: hashlib.sha256((lab.root / name).read_bytes()).hexdigest() for name in files}
        inputs.update(base=base, base_id=original['Id'])
        fingerprint = canonical(inputs)
        image = 'agentbox/image-worker:' + fingerprint[:16]
        path = lab.state_dir / 'batch-image-bundle.json'
        previous = json.loads(path.read_text()) if path.exists() else None
        reused = bool(previous and previous['inputs'] == inputs)
        if reused:
            require(docker_image(lab, image)['Id'] == previous['image_id'], 'batch_image_changed', 'cached worker image ID differs')
            image_id = previous['image_id']
        else:
            with tempfile.TemporaryDirectory(prefix='batch-image-', dir=lab.state_dir) as tmp:
                context = Path(tmp)
                (context / 'images').mkdir()
                (context / 'wheels').mkdir()
                manifest = json.loads((cache / 'images.json').read_text())
                for item in manifest['images']:
                    shutil.copyfile(cache / item['name'], context / 'images' / item['name'])
                from lib.manifests import read_yaml
                wheel = read_yaml(lab.config / 'agentbox/batch-images/versions.lock.yaml')['wheel']['filename']
                shutil.copyfile(cache / wheel, context / 'wheels' / wheel)
                shutil.copyfile(cache / 'images.json', context / 'images.json')
                shutil.copyfile(lab.root / 'workloads/images/worker.py', context / 'worker.py')
                shutil.copyfile(lab.root / 'scripts/lib/product/image_pipeline.py', context / 'image_pipeline.py')
                shutil.copyfile(lab.config / 'agentbox/batch-images/worker.Dockerfile', context / 'Dockerfile')
                lab.runner.run([lab.tools['docker'], 'build', '--pull=false', '--network=none', '--build-arg',
                                'PYTHON_IMAGE=' + base, '-t', image, context], timeout=300)
            image_id = docker_image(lab, image)['Id']
        reference = ("import runpy,json;from pathlib import Path;w=runpy.run_path('/opt/worker.py');"
            "m=json.loads(Path('/opt/images.json').read_text());"
            "print(json.dumps([{'name':n,'pixel_sha256':w['sha'](im.tobytes()),'width':im.width,'height':im.height} "
            "for n,im in w['outputs'](Path('/opt/images'),m)]))")
        computed = json.loads(lab.runner.run([lab.tools['docker'], 'run', '--rm', '--read-only', '--network=none',
            '--memory=256m', '--cpus=.5', '--entrypoint=python3', image, '-I', '-B', '-c', reference], timeout=60))
        require(computed == contract['outputs'], 'batch_pixel_contract_changed', 'target image codec output differs from the reviewed pixel contract')
        imported = import_image(lab, image, image_id, state['cluster'] + '-worker',
                                lab.state_dir / ('batch-image-' + fingerprint[:16] + '.tar'))
        inventory = json.loads(lab.runner.run([lab.tools['docker'], 'exec', state['cluster'] + '-worker',
            'crictl', 'images', '-o', 'json'], announce=False))['images']
        matches = [v for v in inventory if v['id'] == image_id and 'docker.io/' + image in v.get('repoTags', [])]
        require(len(matches) == 1, 'batch_image_identity_missing', 'CRI tag/config identity differs')
        descriptor = bind_digest(lab,state['cluster']+'-worker',image,image_id,create_alias=False)
        result = {'schema_version': 1, 'inputs': inputs, 'fingerprint': fingerprint, 'image': image,
                  'image_id': image_id, 'image_digest': descriptor['manifest_digest'], 'dataset': contract['dataset_id'], 'dataset_sha256': contract['dataset_sha256'],
                  'docker_id': state['docker_id'], 'reused': reused, 'node_cache_reused': imported}
        lab.write_json(path, result)
        lab.write_json(lab.runner.output / 'batch-image-build.json', result)
        return result


def worker_bundle(lab):
    value = json.loads((lab.state_dir / 'batch-image-bundle.json').read_text())
    for name, digest in value['inputs'].items():
        if name not in ('base', 'base_id'):
            require((lab.root/name).is_file() and hashlib.sha256((lab.root/name).read_bytes()).hexdigest() == digest,
                    'batch_image_inputs_changed', 'rebuild the image worker for current inputs')
    require(docker_image(lab, value['image'])['Id'] == value['image_id'], 'batch_image_changed', 'worker image was replaced')
    require(re.fullmatch('sha256:[a-f0-9]{64}', value.get('image_digest', '')) is not None,
            'batch_image_identity_missing', 'rerun batch-image-build to bind the CRI manifest digest')
    state, _ = lab.owned(complete=True)
    descriptor = bind_digest(lab,state['cluster']+'-worker',value['image'],value['image_id'],create_alias=False)
    if descriptor['manifest_digest'] != value['image_digest']:
        value['recorded_manifest_digest'] = value['image_digest']
        value['image_digest'] = descriptor['manifest_digest']
    return value


