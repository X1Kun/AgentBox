import hashlib
from pathlib import Path
import re
from lib.core.paths import ROOT
FIXTURES=ROOT/'tests/fixtures/platform'
from .config import FIXTURES, ROOT, canonical, load_lock, normalized, read_yaml
from .render import render
def build_input_identity(config, name, digest):
    bridge=read_yaml(Path(config)/'lab/format-compatibility.yaml').get('build_inputs',{}).get(name,{})
    return bridge['legacy_sha256'] if digest==bridge.get('current_sha256') else digest


def lock_identity(config,name):
    value = load_lock(config, name)
    identity = canonical(value)
    bridge=read_yaml(Path(config)/'lab/format-compatibility.yaml')['locks'].get(name,{})
    return bridge['legacy_sha256'] if identity==bridge.get('semantic_sha256') else identity


def definition(config,name):
    config=Path(config)
    if name == 'session':
        return {'schema_version': 3, 'locks': {'session': lock_identity(config, 'session')},
                'files': {'components/agent-sandbox/controller-lab-patch.yaml': canonical(read_yaml(config/'components/agent-sandbox/controller-lab-patch.yaml', mapping=False))}}
    specs={'runtime':(['gvisor'],['runtime'],['components/gvisor/runsc.toml']),
      'network':(['calico'],[],['lab/kind-calico.yaml','components/calico/lab-patch.yaml'])}
    locks,selections,files=specs[name]
    current={'schema_version':2,'locks':{n:lock_identity(config,n) for n in locks},
      'profiles':{n:canonical(normalized(render(config,n))) for n in selections},
      'files':{p:(canonical(read_yaml(config/p,mapping=False)) if p.endswith('-patch.yaml') else hashlib.sha256((config/p).read_bytes()).hexdigest()) for p in files}}
    old=read_yaml(config/'lab/format-compatibility.yaml')['definitions'].get(name,{})
    return old['legacy'] if current==old.get('current') else current


def session_definition_matches(config, recorded):
    current = definition(config, 'session')
    if recorded == current:
        return True
    old = read_yaml(Path(config)/'lab/format-compatibility.yaml')['definitions']['session']
    if recorded == old['legacy']:
        return all(current[k] == old['current'][k] for k in ('locks', 'files'))
    if not isinstance(recorded, dict) or recorded.get('schema_version') != 2 or set(recorded) != {'schema_version','locks','profiles','files'}:
        return False
    profiles = recorded.get('profiles', {})
    if not isinstance(profiles, dict):
        return False
    return (set(profiles) == {'session','sdk-client','sdk-rbac'} and
            all(isinstance(v, str) and re.fullmatch('[a-f0-9]{64}', v) for v in profiles.values()) and
            all(current[k] == recorded[k] for k in ('locks', 'files')))


def acceptance_definition(config, fixture_root=FIXTURES):
    return {'schema_version': 3, 'profiles': {name: canonical(normalized(render(config, name, fixture_root=fixture_root)))
            for name in ('sdk-client','sdk-rbac')},
            'client_dockerfile': hashlib.sha256((Path(fixture_root)/'sessions/client.Dockerfile').read_bytes()).hexdigest()}


