import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import yaml
from lib.core.paths import ROOT
FIXTURES=ROOT/'tests/fixtures/platform'
LOCK_PATHS = {'versions':'lab/versions.lock.yaml','gvisor':'components/gvisor/versions.lock.yaml',
              'calico':'components/calico/versions.lock.yaml','session':'components/agent-sandbox/versions.lock.yaml',
              'kueue':'components/kueue/versions.lock.yaml'}


class ManifestError(ValueError):
    reason = 'manifest_render_failed'


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader,node,deep=False):
    loader.flatten_mapping(node)
    result = {}
    for key_node,value_node in node.value:
        key=loader.construct_object(key_node,deep=deep)
        if key in result:
            raise ManifestError('duplicate YAML key: ' + str(key))
        result[key]=loader.construct_object(value_node,deep=deep)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,unique_mapping)


def read_yaml(path, mapping=True):
    path=Path(path)
    if path.stat().st_size > 4 << 20:
        raise ManifestError('YAML configuration is too large')
    try:
        value=yaml.load(path.read_text(),Loader=UniqueLoader)
        json.dumps(value,allow_nan=False)
    except ManifestError:raise
    except (yaml.YAMLError,TypeError,ValueError) as error:
        raise ManifestError('invalid YAML/JSON-compatible configuration: '+str(path)) from error
    if mapping and (not isinstance(value, dict)):
        raise ManifestError('YAML must be an object: ' + str(path))
    return value


def load_lock(config, name):
    return read_yaml(Path(config) / LOCK_PATHS[name])


def repository_image_source(config, name, image):
    sources = read_yaml(Path(config) / 'lab/image-sources.yaml')
    repository = sources.get(name + '_repository')
    if (sources.get('schema_version') != 1 or not isinstance(repository, str) or
            not re.fullmatch(r'[a-z0-9][a-z0-9.:-]*/[a-z0-9._/-]+', repository) or
            '..' in repository or '//' in repository):
        raise ManifestError('invalid image repository in lab/image-sources.yaml: ' + name)
    digest = image.rpartition('@')[2]
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
        raise ManifestError('image requires a locked sha256 digest: ' + name)
    return repository + '@' + digest


def python_image_source(config):
    return repository_image_source(config, 'python', load_lock(config, 'versions')['smoke']['image'])


def canonical(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()).hexdigest()


def objects(value):
    return value['items'] if value.get('kind') == 'List' else [value]


def bundle(values):
    return values[0] if len(values) == 1 else {'apiVersion': 'v1', 'kind': 'List', 'items': values}


def normalized(value):
    return sorted(copy.deepcopy(objects(value)), key=lambda v: (v['apiVersion'], v['kind'], v['metadata'].get('namespace', ''), v['metadata']['name']))


def executable():
    if os.environ.get('AGENTBOX_KUBECTL'):
        return os.environ['AGENTBOX_KUBECTL']
    local=ROOT/'.local/platform-tools/kubectl'
    return str(local) if local.is_file() else shutil.which('kubectl') or 'kubectl'


