import re
from decimal import Decimal
from lib.core.validation import require
from lib.core.labels import OWNER, BATCH_RUN as RUN

def owner_reference(obj, uid, kind=None):
    return any(r.get('uid') == uid and r.get('controller') is True and
               (kind is None or r.get('kind') == kind) for r in obj['metadata'].get('ownerReferences', []))

def condition(obj, name, status='True'):
    return obj is not None and any(c['type'] == name and c['status'] == status for c in obj.get('status', {}).get('conditions', []))

def identity(obj, uid, owner, run=None):
    meta = obj['metadata']
    require(meta.get('uid') == uid and meta.get('labels', {}).get(OWNER) == owner and
            (run is None or meta['labels'].get(RUN) == run),
            'batch_identity_changed', meta['name'])

def quantity(value):
    """Exact CPU/memory quantity arithmetic for budget checks, never float rounding."""
    match = re.fullmatch(r'([0-9]+(?:\.[0-9]+)?)([numkKMGTPE]i?|)', str(value))
    require(match is not None, 'invalid_batch_quantity', str(value))
    number, suffix = match.groups()
    scales = {'': 1, 'n': Decimal('0.000000001'), 'u': Decimal('0.000001'), 'm': Decimal('.001'),
              'k': 1000, **{x: 1000 ** i for i, x in enumerate('KMGTPE', 1)},
              **{x + 'i': 1024 ** i for i, x in enumerate('KMGTPE', 1)}}
    require(suffix in scales, 'invalid_batch_quantity', str(value))
    return Decimal(number) * scales[suffix]


import json
API = 'kueue.x-k8s.io/v1beta2'

def delete_resource(lab, kind, name, uid, namespace=None):
    path = '/apis/' + API + '/' + ('namespaces/' + namespace + '/' if namespace else '')
    resource = {'ClusterQueue': 'clusterqueues', 'ResourceFlavor': 'resourceflavors', 'LocalQueue': 'localqueues'}.get(kind)
    if kind == 'Job':
        path, resource = '/apis/batch/v1/namespaces/' + namespace + '/', 'jobs'
    if kind == 'Namespace':
        path, resource = '/api/v1/', 'namespaces'
    require(resource is not None, 'invalid_batch_cleanup', kind)
    options = {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'propagationPolicy': 'Foreground', 'preconditions': {'uid': uid}}
    lab.kubectl('delete', '--raw', path + resource + '/' + name, '-f', '-', input_text=json.dumps(options), announce=False)
