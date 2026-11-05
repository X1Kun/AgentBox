"""Complete application bundles from explicit non-secret inputs and private credentials."""

import base64
import json
import os
from pathlib import Path
import re
import ssl
import subprocess

import yaml

from lib.manifests.network import add_api_egress, discover_api_endpoints, endpoint_rules
from lib.manifests import ManifestError, executable, objects, render, render_product_template


def dns(value):
    return isinstance(value, str) and re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?', value)


def digest(value):
    return isinstance(value, str) and re.fullmatch('sha256:[a-f0-9]{64}', value) and value != 'sha256:'+'0'*64


def validate_inputs(value):
    common = {'schema_version','product','owner','instance','namespaces','images','api_server_endpoints'}
    product = value.get('product')
    required = common | ({'worker_config_id','worker_manifest_digest'} if product == 'batch' else set())
    if product not in ('session', 'batch') or set(value) != required or value.get('schema_version') != 1:
        raise ManifestError('invalid product input fields')
    if not dns(value['instance']) or not isinstance(value['owner'], str) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9_.-]{0,61}[A-Za-z0-9]', value['owner']):
        raise ManifestError('product owner/instance is invalid')
    namespaces = value['namespaces']
    keys = {'work','api'} if product == 'session' else {'work'}
    if not isinstance(namespaces, dict) or set(namespaces) != keys or not all(dns(v) for v in namespaces.values()) or len(set(namespaces.values())) != len(namespaces):
        raise ManifestError('product namespaces must be distinct explicit DNS names')
    images = value['images']
    keys = {'api','runtime'} if product == 'session' else {'api','worker'}
    if not isinstance(images, dict) or set(images) != keys:
        raise ManifestError('missing product image inputs')
    for image in images.values():
        if not isinstance(image, str) or not re.fullmatch(r'[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}', image) or not digest(image.rsplit('@', 1)[1]):
            raise ManifestError('product images require non-placeholder sha256 references')
    if product == 'batch' and (not digest(value['worker_config_id']) or not digest(value['worker_manifest_digest']) or
                               images['worker'].rsplit('@',1)[1] != value['worker_manifest_digest']):
        raise ManifestError('worker config ID and matching manifest digest are required')
    try:
        endpoint_rules(value['api_server_endpoints'])
    except (ValueError, TypeError, KeyError) as error:
        raise ManifestError('invalid explicit API server endpoints') from error
    return value


def credentials(directory):
    directory = Path(directory)
    data = {}
    for name in ('tls.crt','tls.key','principals.json'):
        path = directory/name
        if path.is_symlink() or not path.is_file() or path.stat().st_size > 512 << 10:
            raise ManifestError('credentials must be bounded regular files')
        if name != 'tls.crt' and path.stat().st_mode & 0o077:
            raise ManifestError('private credential files require mode 0600')
        data[name] = path.read_bytes()
    def unique(pairs):
        result = {}
        for k,v in pairs:
            if k in result:
                raise ValueError('duplicate credential field')
            result[k] = v
        return result
    try:
        principals = json.loads(data['principals.json'], object_pairs_hook=unique)
        if not isinstance(principals,list) or not 1 <= len(principals) <= 64:
            raise ValueError()
        tokens = set()
        for p in principals:
            if not isinstance(p,dict) or set(p) != {'id','token'} or not isinstance(p['id'],str) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9_-]{0,62}',p['id']):
                raise ValueError()
            token = p['token']
            if not isinstance(token,str) or not 32 <= len(token) <= 4096 or any(ord(c)<33 or ord(c)>126 for c in token) or token in tokens:
                raise ValueError()
            tokens.add(token)
        ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(directory/'tls.crt', directory/'tls.key')
        checked = subprocess.run(['openssl','x509','-checkend','0','-noout','-in',directory/'tls.crt'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
        if checked.returncode:
            raise ValueError()
    except (ValueError, TypeError, KeyError, ssl.SSLError, OSError, subprocess.TimeoutExpired) as error:
        raise ManifestError('invalid principals or TLS credential pair') from error
    return data


def compose(config, value, private):
    validate_inputs(value)
    if set(private) != {'tls.crt','tls.key','principals.json'} or not all(isinstance(v,bytes) and v for v in private.values()):
        raise ManifestError('complete private credential material is required')
    params = {'OWNER':value['owner'], 'RUN':value['instance'], 'API_INSTANCE':value['instance'], 'API_IMAGE':value['images']['api']}
    if value['product'] == 'session':
        params.update(WORK_NS=value['namespaces']['work'], CLIENT_NS=value['namespaces']['api'], RUNTIME_IMAGE=value['images']['runtime'])
        secret_namespace = params['CLIENT_NS']
    else:
        params.update(NAMESPACE=value['namespaces']['work'], QUEUE_NAME=value['instance'], FLAVOR_NAME=value['instance']+'-worker',
                      WORKER_IMAGE=value['images']['worker'], WORKER_IMAGE_ID=value['worker_config_id'], WORKER_IMAGE_DIGEST=value['worker_manifest_digest'])
        secret_namespace = params['NAMESPACE']
    resources = objects(render_product_template(config, value['product'], params))
    policy_name = 'session-client' if value['product'] == 'session' else 'batch-api'
    policy = next(o for o in resources if o['kind']=='NetworkPolicy' and o['metadata']['name']==policy_name)
    add_api_egress(policy, value['api_server_endpoints'])
    run_label = 'agentbox.dev/session-run' if value['product']=='session' else 'agentbox.dev/batch-run'
    resources.append({'apiVersion':'v1', 'kind':'Secret', 'metadata':{'name':'agentbox-api-credentials', 'namespace':secret_namespace,
        'labels':{'agentbox.dev/session':value['owner'],run_label:value['instance']}},
        'type':'Opaque','data':{name:base64.b64encode(data).decode() for name,data in private.items()}})
    budgets = [o for o in resources if o['kind']=='ResourceQuota']
    if len(budgets) != 1 or (value['product']=='batch' and budgets[0]['metadata']['name']!='batch-product-budget'):
        raise ManifestError('product must contain exactly its own budget')
    if any(o['kind']=='Role' and o['metadata']['name']=='agentbox-cleanup' for o in resources):
        raise ManifestError('administrator cleanup permissions are not a product dependency')
    order = {'Namespace':0,'ServiceAccount':1,'Secret':1,'ResourceQuota':2,'NetworkPolicy':2,'Role':2,'RoleBinding':3,
             'PersistentVolumeClaim':4,'ConfigMap':4,'ResourceFlavor':5,'ClusterQueue':5,'LocalQueue':6,
             'SandboxTemplate':5,'SandboxWarmPool':6,'Service':7,'Deployment':8}
    return sorted(resources, key=lambda o:order[o['kind']])


def validate_cluster(config, value, kubeconfig, context):
    """Read-only prerequisite/ownership check, always with an explicit context."""
    validate_inputs(value)
    prefix = [executable(),'--kubeconfig',str(kubeconfig),'--context',context,'--request-timeout=5s']
    def get(*args):
        result = subprocess.run(prefix+list(args), capture_output=True, text=True, timeout=8)
        if result.returncode:
            raise ManifestError('cluster prerequisite query failed')
        return result.stdout
    run_label = 'agentbox.dev/session-run' if value['product']=='session' else 'agentbox.dev/batch-run'
    for namespace in value['namespaces'].values():
        raw = get('get','namespace',namespace,'--ignore-not-found','-o','json')
        if raw.strip():
            labels = json.loads(raw)['metadata'].get('labels',{})
            if labels.get('agentbox.dev/session') != value['owner'] or labels.get(run_label) != value['instance']:
                raise ManifestError('existing namespace belongs to another deployment')
    runtime = json.loads(get('get','runtimeclass','agentbox-gvisor','-o','json'))
    expected = render(config, 'runtime', {'OWNER':value['owner']})
    if runtime['handler'] != expected['handler'] or runtime['metadata'].get('labels',{}).get('agentbox.dev/session') != value['owner']:
        raise ManifestError('owned gVisor RuntimeClass prerequisite is missing')
    required = ['sandboxes.agents.x-k8s.io','sandboxclaims.extensions.agents.x-k8s.io',
                'sandboxtemplates.extensions.agents.x-k8s.io','sandboxwarmpools.extensions.agents.x-k8s.io'] if value['product']=='session' else ['workloads.kueue.x-k8s.io','localqueues.kueue.x-k8s.io','clusterqueues.kueue.x-k8s.io','resourceflavors.kueue.x-k8s.io']
    for name in required:
        crd = json.loads(get('get','crd',name,'-o','json'))
        if not any(c['type']=='Established' and c['status']=='True' for c in crd.get('status',{}).get('conditions',[])):
            raise ManifestError('required CRD is not established')
    namespace, name = ('agent-sandbox-system','agent-sandbox-controller') if value['product']=='session' else ('kueue-system','kueue-controller-manager')
    deployment = json.loads(get('-n',namespace,'get','deployment',name,'-o','json'))
    if not any(c['type']=='Available' and c['status']=='True' for c in deployment.get('status',{}).get('conditions',[])):
        raise ManifestError('required controller is not available')
    calico = json.loads(get('-n','kube-system','get','daemonset','calico-node','-o','json'))
    status = calico.get('status',{})
    if not status.get('desiredNumberScheduled') or status.get('numberReady') != status['desiredNumberScheduled']:
        raise ManifestError('lab requires ready Calico NetworkPolicy enforcement')
    actual = discover_api_endpoints(get)
    if {json.dumps(r,sort_keys=True) for r in endpoint_rules(actual)} != {json.dumps(r,sort_keys=True) for r in endpoint_rules(value['api_server_endpoints'])}:
        raise ManifestError('declared API server endpoints differ from the cluster')
    return {'runtimeclass':'agentbox-gvisor','controller':name,'network':'calico','crds':required}


def write_bundle(path, resources):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(path, os.O_CREAT|os.O_EXCL|os.O_WRONLY, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            yaml.safe_dump_all(resources, stream, sort_keys=False)
    except BaseException:
        path.unlink()
        raise
