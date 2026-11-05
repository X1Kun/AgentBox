"""Bind existing owned-node image descriptors to immutable CRI references."""

import hashlib
import json
import re
import time

import yaml

from lib.product.api_image import build_api_image
from lib.manifests.network import discover_api_endpoints
from lib.core.validation import require
from lib.platform.sandbox import Sessions


def bind_digest(lab, node, name, config_id, create_alias=True):
    first = name.split('/')[0]
    source = name if first == 'localhost' or '.' in first or ':' in first else 'docker.io/'+name
    def command(*args):
        return lab.runner.run([lab.tools['docker'],'exec',node,*args],announce=False)
    def descriptor(ref):
        lines=command('ctr','-n','k8s.io','images','ls','name=='+ref).splitlines()
        rows=[line.split() for line in lines[1:] if line.split() and line.split()[0]==ref]
        if not rows:return None
        require(len(rows)==1 and re.fullmatch('sha256:[a-f0-9]{64}',rows[0][2]),'product_image_descriptor_invalid',ref)
        return rows[0][2]
    manifest_digest=descriptor(source)
    require(manifest_digest,'product_image_missing',source)
    def content(digest):
        raw=command('ctr','-n','k8s.io','content','get',digest)
        require('sha256:'+hashlib.sha256(raw.encode()).hexdigest()==digest,'product_image_content_changed',digest)
        return json.loads(raw)
    manifest=content(manifest_digest)
    if 'manifests' in manifest:
        matches=[v for v in manifest['manifests'] if v.get('platform',{}).get('os')=='linux' and v['platform'].get('architecture')=='amd64']
        require(len(matches)==1,'product_image_platform_ambiguous',source)
        manifest=content(matches[0]['digest'])
    require(manifest.get('config',{}).get('digest')==config_id,'product_image_config_changed',source)
    repository=source.split('@',1)[0]
    if ':' in repository.rsplit('/',1)[-1]:repository=repository.rsplit(':',1)[0]
    reference=repository+'@'+manifest_digest
    result={'reference':reference,'config_id':config_id,'manifest_digest':manifest_digest}
    if not create_alias:
        return result
    previous=descriptor(reference)
    require(previous in (None,manifest_digest),'product_image_alias_changed',reference)
    if previous is None:
        command('ctr','-n','k8s.io','images','tag',source,reference)
        command('ctr','-n','k8s.io','images','label',reference,'io.cri-containerd.image=managed')
    until=time.monotonic()+30
    while True:
        images=json.loads(command('crictl','images','-o','json'))['images']
        if any(v['id']==config_id and reference in v.get('repoDigests',[]) for v in images):
            return result
        require(time.monotonic()<until,'product_image_alias_not_visible',reference)
        time.sleep(.2)


def prepare_product_images(lab, with_batch=False):
    with lab.operation_lock():
        sessions=Sessions(lab);runtime=sessions.attach();bundle=sessions.bundle(require_client=False);sessions.verify(bundle)
        prepared=build_api_image(lab,sessions)
        image=prepared['api_image']
        worker=runtime.worker
        control=next(node for node in sessions.state['node_ids'] if node!=worker)
        # The API builder imports the control-plane image. Import the exact same
        # archive into the worker only when its verified config ID is absent.
        inventory=json.loads(runtime.node('crictl','images','-o','json'))['images']
        if not any(v['id']==image['id'] and 'docker.io/'+image['name'] in v.get('repoTags',[]) for v in inventory):
            archive=lab.state_dir/'product-api-image.tar'
            lab.runner.run([lab.tools['docker'],'save','-o',archive,image['name']],timeout=300)
            lab.runner.run([lab.tools['kind'],'load','image-archive',archive,'--name',sessions.state['cluster'],'--nodes',runtime.worker_name],timeout=300)
        api=bind_digest(lab,control,image['name'],image['id'])
        require(bind_digest(lab,worker,image['name'],image['id'])==api,'product_api_node_identity_differs','API descriptors differ')
        sandbox=bind_digest(lab,worker,bundle['images']['runtime']['name'],bundle['images']['runtime']['id'])
        owner=sessions.state['session'];endpoints=discover_api_endpoints(lab.kubectl)
        session_input={'schema_version':1,'product':'session','owner':owner,'instance':'session-lab',
            'namespaces':{'work':'agentbox-session-work','api':'agentbox-session-api'},
            'images':{'api':api['reference'],'runtime':sandbox['reference']},'api_server_endpoints':endpoints}
        result={'api':api,'runtime':sandbox,'api_build':prepared,'inputs':{'session':session_input}}
        if with_batch:
            value=json.loads((lab.state_dir/'batch-image-bundle.json').read_text())
            require(value['docker_id']==sessions.state['docker_id'],'product_worker_daemon_changed','worker bundle belongs to another daemon')
            for name,digest in value['inputs'].items():
                if name not in ('base','base_id'):
                    require((lab.root/name).is_file() and hashlib.sha256((lab.root/name).read_bytes()).hexdigest()==digest,'product_worker_inputs_changed',name)
            selected=bind_digest(lab,worker,value['image'],value['image_id'])
            selected['recorded_manifest_digest']=value['image_digest']
            result['worker']=selected
            result['inputs']['batch']={'schema_version':1,'product':'batch','owner':owner,'instance':'batch-lab',
                'namespaces':{'work':'agentbox-batch-product'},'images':{'api':api['reference'],'worker':selected['reference']},
                'worker_config_id':selected['config_id'],'worker_manifest_digest':selected['manifest_digest'],'api_server_endpoints':endpoints}
        for product,value in result['inputs'].items():
            with (lab.runner.output/(product+'-inputs.yaml')).open('x') as stream:yaml.safe_dump(value,stream,sort_keys=False)
        lab.write_json(lab.runner.output/'product-images.json',result)
        return result
