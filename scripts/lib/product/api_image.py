"""Shared product API image build; independent of acceptance fixtures."""

import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import time
from lib.platform.sandbox import checked_source
from lib.core.errors import SessionError


def ensure(value, message):
    if not value:
        raise SessionError('api_image_preparation_failed', message)


def build_api_image(lab, sessions):
    directory=lab.state_dir/'api-build';directory.mkdir(exist_ok=True)
    binary=directory/'agentbox-api'
    lab.runner.run(['env','CGO_ENABLED=0','go','build','-p=2','-mod=readonly','-trimpath',
        '-buildvcs=false','-ldflags=-s -w','-o',binary,'./cmd/agentbox-api'],timeout=900)
    base=checked_source(lab.lock['smoke']['image'],sessions.bundle(require_client=False)['python_source'])
    base_image=sessions.docker_image(base)  # Require the verified cached source; no implicit pull.
    ensure(base in base_image.get('RepoDigests',[]),'cached API base digest not confirmed')
    inputs={'binary':hashlib.sha256(binary.read_bytes()).hexdigest(),
            'dockerfile':hashlib.sha256((lab.config/'agentbox/api/Dockerfile').read_bytes()).hexdigest(),
            'base':base,'base_id':base_image['Id']}
    fingerprint=hashlib.sha256(json.dumps(inputs,sort_keys=True).encode()).hexdigest()
    image='agentbox/api:'+fingerprint[:16]
    # Send only the binary and Dockerfile, never previous image archives.
    with tempfile.TemporaryDirectory(prefix='api-image-',dir=lab.state_dir) as tmp:
        context=Path(tmp)
        shutil.copyfile(lab.config/'agentbox/api/Dockerfile',context/'Dockerfile')
        shutil.copy2(binary,context/'agentbox-api')
        lab.runner.run([lab.tools['docker'],'build','--pull=false','--network=none','--build-arg',
            'BASE_IMAGE='+base,'-t',image,context],timeout=300)
    identity=sessions.docker_image(image)['Id']
    node=sessions.state['cluster']+'-control-plane'
    def imported():
        inventory=json.loads(lab.runner.run([lab.tools['docker'],'exec',node,'crictl','images','-o','json']))['images']
        return any(v['id']==identity and 'docker.io/'+image in v.get('repoTags',[]) for v in inventory)
    cached=imported()
    if not cached:
        processes=lab.runner.run([lab.tools['docker'],'exec',node,'ps','-eo','comm='])
        ensure('ctr' not in processes.split(),'existing node image import must finish before API image import')
        archive=directory/('api-'+fingerprint[:16]+'.tar')
        lab.runner.run([lab.tools['docker'],'save','-o',archive,image],timeout=300)
        lab.runner.run([lab.tools['kind'],'load','image-archive',archive,'--name',sessions.state['cluster'],
            '--nodes',node],timeout=300)
        until=time.monotonic()+60
        while not imported():
            ensure(time.monotonic()<until,'API image ID did not converge in the CRI cache')
            time.sleep(.5)
    return {'api_image_node_cache_reused':cached,'api_image':{'name':image,'id':identity,'inputs':inputs,'fingerprint':fingerprint}}
