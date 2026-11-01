"""Component-oriented static YAML; Kustomize composes, Python tags each experiment."""
from contextlib import contextmanager
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

try:
    import yaml
except ImportError as error:
    raise RuntimeError('Install scripts/requirements-platform.txt before platform commands') from error

ROOT = Path(__file__).resolve().parents[2]
LOCK_PATHS = {'versions':'lab/versions.lock.yaml','gvisor':'components/gvisor/versions.lock.yaml',
              'calico':'components/calico/versions.lock.yaml','session':'components/agent-sandbox/versions.lock.yaml'}


class ManifestError(ValueError):
    reason = 'manifest_render_failed'


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader,node,deep=False):
    loader.flatten_mapping(node);result={}
    for key_node,value_node in node.value:
        key=loader.construct_object(key_node,deep=deep)
        if key in result:raise ManifestError('duplicate YAML key: '+str(key))
        result[key]=loader.construct_object(value_node,deep=deep)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,unique_mapping)


def read_yaml(path, mapping=True):
    path=Path(path)
    if path.stat().st_size>4<<20:raise ManifestError('YAML configuration is too large')
    try:
        value=yaml.load(path.read_text(),Loader=UniqueLoader)
        json.dumps(value,allow_nan=False)
    except ManifestError:raise
    except (yaml.YAMLError,TypeError,ValueError) as error:
        raise ManifestError('invalid YAML/JSON-compatible configuration: '+str(path)) from error
    if mapping and not isinstance(value,dict):raise ManifestError('YAML must be an object: '+str(path))
    return value


def load_lock(config,name):return read_yaml(Path(config)/LOCK_PATHS[name])


def python_image_source(config):
    sources = read_yaml(Path(config) / 'lab/image-sources.yaml')
    repository = sources.get('python_repository')
    if (sources.get('schema_version') != 1 or not isinstance(repository, str) or
            not re.fullmatch(r'[a-z0-9][a-z0-9.:-]*/[a-z0-9._/-]+', repository) or
            '..' in repository or '//' in repository):
        raise ManifestError('invalid Python image repository in lab/image-sources.yaml')
    image = load_lock(config, 'versions')['smoke']['image']
    digest = image.rpartition('@')[2]
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
        raise ManifestError('Python image requires a locked sha256 digest')
    return repository + '@' + digest


def canonical(value):return hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':'),ensure_ascii=False).encode()).hexdigest()
def objects(value):return value['items'] if value.get('kind')=='List' else [value]
def bundle(values):return values[0] if len(values)==1 else {'apiVersion':'v1','kind':'List','items':values}
def normalized(value):return sorted(copy.deepcopy(objects(value)),key=lambda v:(v['apiVersion'],v['kind'],v['metadata'].get('namespace',''),v['metadata']['name']))


def executable():
    if os.environ.get('AGENTBOX_KUBECTL'):return os.environ['AGENTBOX_KUBECTL']
    local=ROOT/'.local/platform-tools/kubectl'
    return str(local) if local.is_file() else shutil.which('kubectl') or 'kubectl'


# These are orchestration selections, not a directory/overlay per script call.
# Resource definitions and environment patches live alongside their components.
APP_LAB='agentbox/overlays/lab'
RECIPES={
 'api':(APP_LAB,[],{('Deployment','agentbox-api')}),
 'api-rbac':(APP_LAB,[],{(k,'agentbox-api') for k in ('ServiceAccount','Role','RoleBinding')}),
 'api-quota':(APP_LAB,[],{('ResourceQuota','agentbox-api-budget')}),
 'cleanup-rbac':(APP_LAB,[],{(k,'agentbox-cleanup') for k in ('ServiceAccount','Role','RoleBinding')}),
 'session':(APP_LAB,[],{('SandboxTemplate','python'),('SandboxWarmPool','python-cold')}),
 'session-warm':(APP_LAB,[('SandboxWarmPool','acceptance/sessions/warm-pool-patch.yaml')],{('SandboxTemplate','python'),('SandboxWarmPool','python-warm')}),
 'runtime':('components/gvisor',[('RuntimeClass','lab/gvisor-patch.yaml')],{('RuntimeClass','agentbox-gvisor')}),
 'gvisor-probe':('acceptance/gvisor',[],{('Job','agentbox-probe')}),
 'smoke':('acceptance/smoke',[],{('Job','agentbox-smoke-preview')}),
 'sdk-client':('acceptance/sessions',[],{('Job','session-client')}),
 'sdk-rbac':('acceptance/sessions',[],{(k,'session-client') for k in ('ServiceAccount','Role','RoleBinding')}),
 'warm-quota':('acceptance/sessions',[],{('ResourceQuota','session-budget')}),
 'client-policy':('acceptance/sessions',[],{('NetworkPolicy','session-client')}),
 'network-pod':('acceptance/network',[],{('Pod','agentbox-probe')}),
 'network-control':('acceptance/network',[('Pod','acceptance/network/control-patch.yaml')],{('Pod','agentbox-probe')}),
 'network-worker':('acceptance/network',[('Pod','acceptance/network/worker-patch.yaml')],{('Pod','agentbox-probe')}),
 'observer':('acceptance/network',[('Pod','acceptance/network/observer-patch.yaml')],{('Pod','agentbox-probe')}),
 'network-policy':('acceptance/network',[],{('NetworkPolicy',name) for name in ('default-deny','allow-control','allow-data','allow-dns')}),
 'platform-namespace':('lab/namespace.yaml',[],{('Namespace','agentbox-platform')}),
 'session-namespace':('acceptance/sessions/namespace.yaml',[],{('Namespace','agentbox-platform')}),
 'network-namespace':('acceptance/network/namespace.yaml',[],{('Namespace','agentbox-platform')}),
}
REQUIRED={
 'runtime':{'OWNER'},'gvisor-probe':{'OWNER','NAME','IMAGE'},'smoke':{'OWNER','RUN_ID','SMOKE_IMAGE'},
 'network-pod':{'OWNER','RUN','NAME','NAMESPACE','ROLE','IMAGE'},'network-control':{'OWNER','RUN','NAME','NAMESPACE','ROLE','IMAGE'},
 'network-worker':{'OWNER','RUN','NAME','NAMESPACE','ROLE','IMAGE'},'observer':{'OWNER','RUN','NAME','NAMESPACE','ROLE','IMAGE'},
 'network-policy':{'OWNER','RUN','NAMESPACE'},'session':{'OWNER','RUN','WORK_NS','CLIENT_NS','RUNTIME_IMAGE'},
 'session-warm':{'OWNER','RUN','WORK_NS','CLIENT_NS','RUNTIME_IMAGE'},'warm-quota':{'OWNER','RUN','WORK_NS'},
 'sdk-client':{'OWNER','RUN','WORK_NS','CLIENT_NS','CLIENT_IMAGE'},'sdk-rbac':{'OWNER','RUN','WORK_NS','CLIENT_NS'},
 'api':{'OWNER','RUN','WORK_NS','CLIENT_NS','API_IMAGE'},'api-rbac':{'OWNER','RUN','WORK_NS','CLIENT_NS'},
 'api-quota':{'OWNER','RUN','WORK_NS'},'cleanup-rbac':{'OWNER','RUN','WORK_NS','CLIENT_NS'},
 'client-policy':{'OWNER','RUN','WORK_NS','CLIENT_NS'},'platform-namespace':{'OWNER','NAME'},
 'session-namespace':{'OWNER','RUN','NAME','ROLE'},'network-namespace':{'OWNER','RUN','NAME','ROLE'},
}
PARAMETERS={'OWNER','RUN','RUN_ID','WORK_NS','CLIENT_NS','NAMESPACE','NAME','ROLE','RUNTIME_IMAGE','CLIENT_IMAGE','API_IMAGE','IMAGE','SMOKE_IMAGE','RUNTIME_NAME','HANDLER','API_INSTANCE'}


def profiles(config):return sorted(RECIPES)


def _validate_tree(directory):
    for path in directory.rglob('kustomization.yaml'):
        value=read_yaml(path)
        refs=value.get('resources',[])+value.get('components',[])+value.get('configurations',[])
        refs += [v['path'] for v in value.get('patches',[]) if 'path' in v]
        for ref in refs:
            if not isinstance(ref,str) or '://' in ref or ref.startswith(('git@','/')) or not (path.parent/ref).resolve().is_relative_to(directory.resolve()):
                raise ManifestError('Kustomize references must stay within the local deploy tree')


@contextmanager
def _stage(config):
    config=Path(config)
    if any(p.is_symlink() for p in config.rglob('*')):raise ManifestError('symlinked manifest inputs are forbidden')
    with tempfile.TemporaryDirectory(prefix='agentbox-kustomize-') as tmp:
        root=Path(tmp);shutil.copytree(config,root/'source');_validate_tree(root)
        yield root


def _build(root,entry,patches=(),images=None):
    k={'apiVersion':'kustomize.config.k8s.io/v1beta1','kind':'Kustomization','resources':['source/'+entry]}
    if patches:k['patches']=[{'target':{'kind':kind},'path':'source/'+path} for kind,path in patches]
    if images:k['images']=images
    (root/'kustomization.yaml').write_text(yaml.safe_dump(k,sort_keys=False))
    try:result=subprocess.run([executable(),'kustomize',str(root)],capture_output=True,text=True,timeout=30,env=dict(os.environ,KUBECONFIG=os.devnull))
    except (OSError,subprocess.TimeoutExpired) as error:raise ManifestError('local Kustomize could not start or timed out') from error
    if result.returncode:raise ManifestError('Kustomize build failed: '+result.stderr[-2000:].strip())
    values=[v for v in yaml.load_all(result.stdout,Loader=UniqueLoader) if v is not None]
    if not values or any(not isinstance(v,dict) or 'apiVersion' not in v for v in values):raise ManifestError('Kustomize returned invalid resources')
    return values


def _parameters(profile,params,strict):
    p=dict(params or {})
    if p.keys()-PARAMETERS:raise ManifestError('unknown runtime parameter')
    if params is not None and strict and not REQUIRED[profile].issubset(p):raise ManifestError('missing runtime parameters for '+profile)
    for key,value in p.items():
        if not isinstance(value,str) or not value or len(value)>1024 or any(ord(c)<32 for c in value):raise ManifestError('invalid scalar parameter: '+key)
        if key in {'OWNER','RUN','RUN_ID','ROLE'} and not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?',value):raise ManifestError('invalid label parameter: '+key)
        if key in {'WORK_NS','CLIENT_NS','NAMESPACE','NAME','RUNTIME_NAME','HANDLER','API_INSTANCE'} and not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?',value):raise ManifestError('invalid DNS parameter: '+key)
    if 'RUN' in p:p.setdefault('API_INSTANCE','api-'+p['RUN'])
    return p


def _labels(value,p,role=False):
    for key in ('agentbox.dev/session-run','agentbox.dev/net-run'):
        if key in value and 'RUN' in p:value[key]=p['RUN']
    if 'agentbox.dev/session' in value and 'OWNER' in p:value['agentbox.dev/session']=p['OWNER']
    if role:
        for key in ('agentbox.dev/session-role','agentbox.dev/net-role'):
            if key in value and 'ROLE' in p:value[key]=p['ROLE']


def _namespace(value,p):
    if value=='agentbox-work':return p.get('WORK_NS',value)
    if value=='agentbox-client':return p.get('CLIENT_NS',value)
    return p.get('NAMESPACE',value) if value=='agentbox-platform' else value


def _pod(pod,metadata,p,profile):
    _labels(metadata.get('labels',{}),p)
    for c in pod.get('containers',[]):
        key={'sandboxd':'RUNTIME_IMAGE','api':'API_IMAGE','client':'CLIENT_IMAGE'}.get(c['name'],'SMOKE_IMAGE' if profile=='smoke' else 'IMAGE')
        if key in p:c['image']=p[key]
        for e in c.get('env',[]):
            source={'NET_RUN':'RUN','SESSION_NAMESPACE':'WORK_NS','SESSION_OWNER':'OWNER','SESSION_RUN':'RUN'}.get(e['name'])
            if source in p:e['value']=p[source]
        for i,arg in enumerate(c.get('args',[])):
            key=arg.partition('=')[0]
            if key=='--namespace' and 'WORK_NS' in p:c['args'][i]='--namespace='+p['WORK_NS']
            if key=='--instance' and 'API_INSTANCE' in p:c['args'][i]='--instance='+p['API_INSTANCE']


def _selectors(value,p):
    if isinstance(value,list):
        for child in value:_selectors(child,p)
    elif isinstance(value,dict):
        for key,child in value.items():
            if key=='matchLabels' and isinstance(child,dict):
                _labels(child,{k:v for k,v in p.items() if k=='RUN'})
                if 'kubernetes.io/metadata.name' in child:child['kubernetes.io/metadata.name']=_namespace(child['kubernetes.io/metadata.name'],p)
            else:_selectors(child,p)


def _inject(values,p,profile):
    for obj in values:
        kind=obj['kind'];meta=obj['metadata'];_labels(meta.get('labels',{}),p,True)
        if 'namespace' in meta:meta['namespace']=_namespace(meta['namespace'],p)
        if kind in ('Namespace','Pod','Job') and 'NAME' in p:meta['name']=p['NAME']
        if profile=='smoke' and 'RUN_ID' in p:meta['name']='agentbox-smoke-'+p['RUN_ID']
        if profile=='observer' and 'RUN' in p:meta.setdefault('labels',{})['agentbox.dev/session-run']=p['RUN']
        if kind=='RuntimeClass':meta['name']=p.get('RUNTIME_NAME',meta['name']);obj['handler']=p.get('HANDLER',obj['handler'])
        if kind=='Pod':_pod(obj['spec'],meta,p,profile)
        if kind in ('Job','Deployment'):_pod(obj['spec']['template']['spec'],obj['spec']['template'].get('metadata',{}),p,profile)
        if kind=='SandboxTemplate':
            template=obj['spec']['podTemplate'];_pod(template['spec'],template.get('metadata',{}),p,profile);_selectors(obj['spec']['networkPolicy'],p)
        if kind=='NetworkPolicy':_selectors(obj['spec'],p)
        if kind=='RoleBinding':
            for subject in obj['subjects']:
                if 'namespace' in subject:subject['namespace']=_namespace(subject['namespace'],p)
    return values


def render(config,profile,params=None,strict=True):
    if profile not in RECIPES:raise ManifestError('unknown experiment resource selection: '+profile)
    p=_parameters(profile,params,strict)
    optional={'runtime':{'RUNTIME_NAME','HANDLER'},'api':{'API_INSTANCE'}}.get(profile,set())
    p={k:v for k,v in p.items() if k in REQUIRED[profile] | optional}
    entry,patches,selected=RECIPES[profile]
    with _stage(config) as root:
        values=[o for o in _build(root,entry,patches) if (o['kind'],o['metadata']['name']) in selected]
    if len(values)!=len(selected):raise ManifestError('component resource selection is incomplete: '+profile)
    return bundle(_inject(values,p,profile))


def render_component(config,name,params=None,environment='lab'):
    entries={'api':'agentbox/api','cleanup':'agentbox/cleanup','agent-sandbox':'components/agent-sandbox','gvisor':'components/gvisor'}
    if name not in entries:raise ManifestError('unknown component')
    if environment=='lab' and name in ('api','cleanup','agent-sandbox'):
        entry=APP_LAB
        names={'api':{'agentbox-api','agentbox-api-budget'},'cleanup':{'agentbox-cleanup'},'agent-sandbox':{'python','python-cold'}}[name]
    else:entry=entries[name];names=None
    patches=[('RuntimeClass','lab/gvisor-patch.yaml')] if name=='gvisor' and environment=='lab' else []
    with _stage(config) as root:values=_build(root,entry,patches)
    if names:values=[o for o in values if o['metadata']['name'] in names]
    return bundle(_inject(values,_parameters('runtime',params,False),name))


def render_fixture(config,name,params=None):
    if name not in ('smoke','gvisor','network','sessions'):raise ManifestError('unknown acceptance fixture')
    with _stage(config) as root:values=_build(root,'acceptance/'+name)
    return bundle(_inject(values,_parameters('runtime',params,False),name))


def lock_identity(config,name):
    value=load_lock(config,name);identity=canonical(value)
    bridge=read_yaml(Path(config)/'lab/format-compatibility.yaml')['locks'].get(name,{})
    return bridge['legacy_sha256'] if identity==bridge.get('semantic_sha256') else identity


def definition(config,name):
    config=Path(config)
    specs={'runtime':(['gvisor'],['runtime'],['components/gvisor/runsc.toml']),
      'network':(['calico'],[],['lab/kind-calico.yaml','components/calico/lab-patch.yaml']),
      'session':(['session'],['session','sdk-rbac','sdk-client'],['components/agent-sandbox/controller-lab-patch.yaml'])}
    locks,selections,files=specs[name]
    current={'schema_version':2,'locks':{n:lock_identity(config,n) for n in locks},
      'profiles':{n:canonical(normalized(render(config,n))) for n in selections},
      'files':{p:(canonical(read_yaml(config/p,mapping=False)) if p.endswith('-patch.yaml') else hashlib.sha256((config/p).read_bytes()).hexdigest()) for p in files}}
    old=read_yaml(config/'lab/format-compatibility.yaml')['definitions'].get(name,{})
    return old['legacy'] if current==old.get('current') else current


def render_upstream(config,component,resources,params,images=None):
    path={'controller-lab':'components/agent-sandbox/controller-lab-patch.yaml','calico-lab':'components/calico/lab-patch.yaml'}[component]
    target='Deployment' if component=='controller-lab' else 'DaemonSet'
    with _stage(config) as root:
        (root/'source/upstream.yaml').write_text(yaml.safe_dump_all(copy.deepcopy(resources),sort_keys=False))
        values=_build(root,'upstream.yaml',[(target,path)],images)
    for obj in values:
        obj['metadata'].setdefault('labels',{}).update({'agentbox.dev/session':params['OWNER'],'app.kubernetes.io/managed-by':'agentbox-platform'})
        if component=='controller-lab' and obj['kind']=='Deployment':obj['spec']['template']['spec']['containers'][0]['image']=params['CONTROLLER_IMAGE']
        if component=='calico-lab' and obj['kind']=='DaemonSet':
            for c in obj['spec']['template']['spec']['containers']:
                if c['name']=='calico-node':
                    for e in c['env']:
                        if e['name']=='CALICO_IPV4POOL_CIDR':e['value']=params['POD_CIDR']
    return bundle(values)
