"""Fixed image pipeline. No network, user code, arbitrary paths or API credentials."""
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sys
import time
import warnings

from PIL import Image, __version__

UID = re.compile(r'^[a-z0-9-]{1,64}$')
MAX_FILE = 256 << 10
MAX_TOTAL = 2 << 20
Image.MAX_IMAGE_PIXELS = 4_000_000
warnings.simplefilter('error', Image.DecompressionBombWarning)


def sha(value):
    return hashlib.sha256(value).hexdigest()


def transform(image):
    image = image.convert('RGB')
    image.thumbnail((128, 128), Image.Resampling.LANCZOS)
    return {'thumb': image, 'gray': image.convert('L').convert('RGB')}


def outputs(root, manifest):
    values = []
    for item in manifest['images']:
        name = item['name']
        if Path(name).name != name or len(name) > 64:
            raise ValueError('invalid image filename')
        path = root / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size != item['bytes'] or item['bytes'] > 4 << 20:
            raise ValueError('input size/type differs')
        raw = path.read_bytes()
        if sha(raw) != item['sha256']:
            raise ValueError('input checksum differs')
        with Image.open(io.BytesIO(raw)) as image:
            image.load()
            if image.size != (item['width'], item['height']):
                raise ValueError('input dimensions differ')
            for kind, value in transform(image).items():
                values.append((Path(name).stem + '-' + kind + '.png', value.copy()))
    return values


def main():
    job_uid, pod_uid = os.environ['JOB_UID'], os.environ['POD_UID']
    if not UID.fullmatch(job_uid) or not UID.fullmatch(pod_uid):
        raise ValueError('invalid attempt identity')
    if sys.argv[1] == 'prepare':
        (Path('/data/attempts') / job_uid / pod_uid).mkdir(parents=True, exist_ok=True, mode=0o770)
        return
    if sys.argv[1] != 'run' or __version__ != '12.2.0' or os.getuid() != 1000:
        raise ValueError('invalid image worker mode/version/identity')
    if not os.statvfs('/').f_flag & os.ST_RDONLY or Path('/var/run/secrets/kubernetes.io/serviceaccount/token').exists():
        raise ValueError('worker security boundary differs')
    passes = int(os.environ['BATCH_PASSES'])
    request = os.environ['BATCH_REQUEST_SHA256']
    if not 1 <= passes <= 4096 or not re.fullmatch('[a-f0-9]{64}', request):
        raise ValueError('invalid bounded task parameters')
    manifest = json.loads(Path('/opt/images.json').read_text())
    until = time.monotonic() + 150
    print(json.dumps({'phase': 'running', 'job_uid': job_uid, 'pod_uid': pod_uid, 'passes': passes}), flush=True)
    for _ in range(passes):
        if time.monotonic() > until:
            raise TimeoutError('image computation deadline')
        values = outputs(Path('/opt/images'), manifest)
    files, total = [], 0
    for name, image in values:
        output = io.BytesIO()
        image.save(output, format='PNG', optimize=False, compress_level=6)
        data = output.getvalue()
        total += len(data)
        if len(data) > MAX_FILE or total > MAX_TOTAL:
            raise ValueError('output exceeds byte budget')
        path = Path('/output') / name
        with path.open('xb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        files.append({'name': name, 'bytes': len(data), 'sha256': sha(data),
                      'pixel_sha256': sha(image.tobytes()), 'width': image.width, 'height': image.height})
    result = {'schema_version': 1, 'dataset_sha256': manifest['dataset_sha256'], 'request_sha256': request,
              'job_uid': job_uid, 'pod_uid': pod_uid, 'passes': passes, 'files': files}
    temporary = Path('/output/result.json.tmp')
    with temporary.open('x') as stream:
        json.dump(result, stream, sort_keys=True)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.rename('/output/result.json')
    directory = os.open('/output', os.O_DIRECTORY)
    os.fsync(directory)
    os.close(directory)
    print(json.dumps({'phase': 'complete', 'job_uid': job_uid, 'pod_uid': pod_uid, 'files': len(files)}), flush=True)


if __name__ == '__main__':
    main()
