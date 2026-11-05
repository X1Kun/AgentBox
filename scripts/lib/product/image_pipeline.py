"""Fixed image transforms and checked inputs; no orchestration or process entry."""
import hashlib
import io
from pathlib import Path
import warnings
from PIL import Image
Image.MAX_IMAGE_PIXELS=4_000_000
warnings.simplefilter("error",Image.DecompressionBombWarning)

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


