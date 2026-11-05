"""Prepare the fixed, checksum-verified dataset and wheel before offline image builds."""

#!/usr/bin/env python3
import json
import urllib.request

import yaml
from PIL import __version__
from lib.manifests import ROOT, canonical, read_yaml
from lib.product.image_pipeline import outputs, sha


def prepare(download=False, cache=None):
    config = ROOT / 'deploy/agentbox/batch-images'
    manifest = read_yaml(config / 'dataset.yaml')
    lock = read_yaml(config / 'versions.lock.yaml')
    if __version__ != lock['pillow_version']:
        raise ValueError('prepare requires the pinned Pillow version')
    cache = cache or ROOT / '.local/batch-images'
    cache.mkdir(parents=True, exist_ok=True)
    for item in [*manifest['images'], dict(lock['wheel'], name=lock['wheel']['filename'])]:
        path = cache / item['name']
        if not path.exists() and download:
            with urllib.request.urlopen(item['url'], timeout=45) as response:
                data = response.read(item['bytes'] + 1)
            if len(data) != item['bytes'] or sha(data) != item['sha256']:
                raise ValueError('download differs: ' + item['name'])
            temporary = path.with_suffix(path.suffix + '.part')
            temporary.write_bytes(data)
            temporary.rename(path)
        if path.is_symlink() or not path.is_file() or path.stat().st_size != item['bytes'] or sha(path.read_bytes()) != item['sha256']:
            raise ValueError('prepare/cache asset differs: ' + item['name'])
    manifest['dataset_sha256'] = canonical(manifest)
    generated = {'schema_version': 1, 'dataset_id': manifest['dataset_id'],
                 'dataset_sha256': manifest['dataset_sha256'], 'outputs': []}
    for name, image in outputs(cache, manifest):
        generated['outputs'].append({'name': name, 'pixel_sha256': sha(image.tobytes()),
                                     'width': image.width, 'height': image.height})
    expected = config / 'contract.yaml'
    if expected.exists() and read_yaml(expected) != generated:
        raise ValueError('generated pixel contract differs from the reviewed contract')
    (cache / 'images.json').write_text(json.dumps(manifest, sort_keys=True) + '\n')
    (cache / 'contract.yaml').write_text(yaml.safe_dump(generated, sort_keys=False))
    return cache, generated


