"""Opt-in AdmissionCheck bundle; never mutates or automatically attaches a queue."""
import copy
import ipaddress
import math
import re
import ssl
import urllib.parse

from lib.manifests import ManifestError, executable
from lib.manifests.network import endpoint_rules
import subprocess
import yaml


def validate_policy(policy):
    required=set(('schema_version namespace local_queue cluster_queue admission_check owner health prometheus_url '
                  'minimum_samples healthy_ratio protect_ratio max_age_seconds stable_seconds protect_seconds '
                  'permit_interval_seconds max_wait_seconds retry_seconds poll_seconds request_timeout_seconds').split())
    if not isinstance(policy,dict) or not required<=set(policy) or set(policy)-required-{'ca_file'} or policy.get('schema_version')!=1:
        raise ManifestError('complete versioned admission policy required')
    health=policy.get('health')
    if not isinstance(health,dict) or set(health)!={'domain','revision','profile','window_seconds','deadline_seconds'}:
        raise ManifestError('complete health contract required')
    for name in ('domain','revision','profile'):
        if not isinstance(health[name],str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,62}',health[name]):
            raise ManifestError('invalid health contract identity')
    for name,low,high in [('window_seconds',1,120),('deadline_seconds',1,60)]:
        if type(health[name]) is not int or not low<=health[name]<=high:
            raise ManifestError('invalid health window/deadline')
    for name in ('max_age_seconds','stable_seconds','protect_seconds','permit_interval_seconds','max_wait_seconds','retry_seconds','poll_seconds','request_timeout_seconds'):
        if type(policy[name]) is not int or not 1<=policy[name]<=3600:
            raise ManifestError('invalid admission budget')
    if type(policy['minimum_samples']) is not int or not 1<=policy['minimum_samples']<=2000:
        raise ManifestError('invalid sample minimum')
    if any(type(policy[k]) not in (int,float) or not math.isfinite(policy[k]) for k in ('protect_ratio','healthy_ratio')) or not 0<=policy['protect_ratio']<policy['healthy_ratio']<=1:
        raise ManifestError('invalid hysteresis thresholds')
    if policy['poll_seconds']>30 or policy['request_timeout_seconds']>10 or policy['max_age_seconds']<health['window_seconds']+health['deadline_seconds'] or policy['max_wait_seconds']<policy['retry_seconds']:
        raise ManifestError('inconsistent admission budgets')


def compose(config, policy, image, controller_namespace, endpoints, health_endpoints, ca_pem=None):
    """Render explicit controller identity, policy, image and network inputs."""
    policy = copy.deepcopy(policy)
    if ca_pem is not None:
        if not isinstance(ca_pem,str) or len(ca_pem.encode())>1<<20 or 'PRIVATE KEY' in ca_pem:
            raise ManifestError('bounded public Prometheus CA certificates required')
        try:
            ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT).load_verify_locations(cadata=ca_pem)
        except (ssl.SSLError,ValueError) as error:
            raise ManifestError('invalid Prometheus CA bundle') from error
        policy['ca_file']='/etc/agentbox/prometheus-ca.pem'
    elif policy.get('ca_file'):
        raise ManifestError('explicit Prometheus CA contents are required for a custom CA path')
    validate_policy(policy)
    names = [controller_namespace, policy.get('namespace'), policy.get('local_queue'),
             policy.get('cluster_queue'), policy.get('admission_check')]
    if not all(isinstance(n, str) and re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', n) for n in names):
        raise ManifestError('invalid admission scope')
    if controller_namespace == policy['namespace']:
        raise ManifestError('controller namespace must be separate from compute scope')
    if not re.fullmatch(r'[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}', image) or image.endswith('0'*64):
        raise ManifestError('admission image requires an immutable non-placeholder digest')
    owner, revision = policy.get('owner'), policy.get('health', {}).get('revision')
    if not all(isinstance(n, str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,62}', n) for n in (owner, revision)):
        raise ManifestError('invalid admission identity/revision')
    url = urllib.parse.urlsplit(policy.get('prometheus_url', ''))
    if url.scheme != 'https' or not url.hostname or url.username is not None or url.query or url.fragment or url.path not in ('', '/') or url.hostname.endswith('.invalid'):
        raise ManifestError('Pod deployment requires an explicit reachable HTTPS Prometheus origin')
    try:
        if ipaddress.ip_address(url.hostname).is_loopback:
            raise ManifestError('Pod cannot query host loopback')
    except ValueError:
        pass
    try:
        rules = endpoint_rules(endpoints) + endpoint_rules(health_endpoints)
        if any(ipaddress.ip_address(e['address']).is_loopback for e in endpoints+health_endpoints):
            raise ValueError('Pod endpoints cannot be loopback')
        if not any(e['port']==(url.port or 443) for e in health_endpoints):
            raise ValueError('Prometheus endpoint port differs from origin')
    except (TypeError, ValueError, KeyError) as error:
        raise ManifestError('explicit API and Prometheus IP endpoints are required') from error
    raw = subprocess.run([executable(), 'kustomize', str(config/'agentbox/admission')],
                         capture_output=True, text=True, timeout=30)
    if raw.returncode:
        raise ManifestError('admission Kustomize rendering failed')
    resources = [v for v in yaml.safe_load_all(raw.stdout) if v]
    replacements = {'agentbox-admission': controller_namespace, 'agentbox-batch': policy['namespace'],
                    'agentbox-health': policy['admission_check'], 'batch': policy['local_queue']}
    # Replace explicit namespace/resource references rather than text in policies.
    for obj in resources:
        meta = obj['metadata']
        if 'namespace' in meta:
            meta['namespace'] = replacements.get(meta['namespace'], meta['namespace'])
        meta.setdefault('labels', {})['agentbox.dev/session'] = owner
        if obj['kind'] in ('ClusterRole', 'ClusterRoleBinding'):
            meta['name'] = controller_namespace
        if obj['kind'] == 'Namespace':
            meta['name'] = controller_namespace
        for subject in obj.get('subjects', []):
            subject['namespace'] = controller_namespace
        if obj['kind'] == 'ClusterRoleBinding':
            obj['roleRef']['name'] = controller_namespace
        for rule in obj.get('rules', []):
            if rule.get('resources') == ['clusterqueues']:
                rule['resourceNames'] = [policy['cluster_queue']]
            elif 'resourceNames' in rule:
                rule['resourceNames'] = [replacements.get(v, v) for v in rule['resourceNames']]
        if obj['kind'] == 'AdmissionCheck':
            meta['name'] = policy['admission_check']
            meta['annotations']['agentbox.dev/policy-revision'] = revision
            meta['annotations']['agentbox.dev/lease-namespace'] = controller_namespace
        if obj['kind'] == 'ConfigMap':
            obj['data']['policy.yaml'] = yaml.safe_dump(policy, sort_keys=False)
            if ca_pem is not None:obj['data']['prometheus-ca.pem']=ca_pem
        if obj['kind'] == 'NetworkPolicy':
            obj['spec']['egress'].extend(rules)
        if obj['kind'] == 'Deployment':
            container = obj['spec']['template']['spec']['containers'][0]
            container['image'] = image
            container['args'][1] = '--lease-namespace='+controller_namespace
            obj['spec']['template']['metadata']['labels']['agentbox.dev/session'] = owner
    return resources
