"""Trusted bounded guest payload for platform-gvisor-test; not a user executor."""
import errno
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

mode = sys.argv[1]
assert mode in {"success", "failure", "cancel", "deadline", "oom"}
assert os.getuid() == 1000
assert not Path('/var/run/secrets/kubernetes.io/serviceaccount/token').exists()
assert os.statvfs('/').f_flag & os.ST_RDONLY, Path('/proc/mounts').read_text()
try:
    Path('/agentbox-write-probe').write_text('must not succeed')
except OSError as error:
    assert error.errno in (errno.EROFS, errno.EACCES), error
else:
    raise AssertionError('root filesystem is writable')
assert not Path('/work/probe').exists()
Path('/work/probe').write_text('isolated writable work directory')
assert Path('/work/probe').read_text() == 'isolated writable work directory'
if mode in {'cancel', 'deadline'}:
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    subprocess.Popen([sys.executable, '-I', '-B', '-c',
                      'import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(120)'])
print(json.dumps({'ready': True, 'uid': os.getuid(), 'readonly_root': True,
                  'serviceaccount_token': False, 'mode': mode}), flush=True)
# Wait for the host to capture runtime identity and resource evidence first.
until = time.monotonic() + 120
while not Path('/work/go').exists():
    if time.monotonic() > until:
        raise TimeoutError('probe activation deadline')
    time.sleep(.1)
if mode == 'success':
    print(json.dumps({'result': sum(i*i for i in range(1, 1001))}), flush=True)
elif mode == 'failure':
    sys.exit(23)
elif mode == 'oom':
    blocks = []
    for _ in range(64):  # Bounded to 1 GiB even if the expected 256 MiB limit is broken.
        block = bytearray(16 * 1024 * 1024)
        for offset in range(0, len(block), 4096):
            block[offset] = 1  # Fault in writable pages; virtual allocation alone is not an OOM test.
        blocks.append(block)
        if len(blocks) % 4 == 0:
            print(json.dumps({'allocated_bytes': len(blocks) * len(block)}), flush=True)
        if len(blocks) == 32:
            time.sleep(10)  # Leave a bounded observation window if the limit is not effective.
        time.sleep(.02)
    raise AssertionError('memory limit did not terminate the allocation probe')
else:
    time.sleep(120)
