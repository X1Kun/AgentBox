"""Build/import the controller from fixed local inputs; no implicit base pull."""
import hashlib
import json
from pathlib import Path
import shutil
import tempfile

from lib.core.validation import require
from lib.product.images import bind_digest
from lib.product.worker_image import import_image
from lib.platform.sandbox import checked_source


def build_admission_image(lab, sessions):
    directory=lab.state_dir/'admission-build';directory.mkdir(exist_ok=True)
    binary=directory/'agentbox-admission'
    lab.runner.run(['env','CGO_ENABLED=0','go','build','-p=2','-mod=readonly','-trimpath','-buildvcs=false',
                    '-ldflags=-s -w','-o',binary,'./cmd/agentbox-admission'],timeout=900)
    base=checked_source(lab.lock['smoke']['image'],sessions.bundle(require_client=False)['python_source'])
    original=sessions.docker_image(base)
    require(base in original.get('RepoDigests',[]),'admission_base_changed','fixed cached base required')
    dockerfile=lab.config/'agentbox/admission/Dockerfile'
    inputs={'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),
            'dockerfile_sha256':hashlib.sha256(dockerfile.read_bytes()).hexdigest(),'base':base,'base_id':original['Id']}
    fingerprint=hashlib.sha256(json.dumps(inputs,sort_keys=True).encode()).hexdigest()
    image='agentbox/admission:'+fingerprint[:16]
    with tempfile.TemporaryDirectory(prefix='admission-image-',dir=lab.state_dir) as tmp:
        context=Path(tmp);shutil.copyfile(dockerfile,context/'Dockerfile');shutil.copy2(binary,context/'agentbox-admission')
        lab.runner.run([lab.tools['docker'],'build','--pull=false','--network=none','--build-arg','BASE_IMAGE='+base,'-t',image,context],timeout=300)
    image_id=sessions.docker_image(image)['Id'];node=sessions.state['cluster']+'-control-plane'
    imported=import_image(lab,image,image_id,node,directory/('admission-'+fingerprint[:16]+'.tar'))
    result={**bind_digest(lab,node,image,image_id),'inputs':inputs,'tag':image,'node':node,'cache_reused':imported}
    lab.write_json(lab.runner.output/'admission-image.json',result)
    return result
