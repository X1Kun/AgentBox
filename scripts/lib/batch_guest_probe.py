"""Bounded CPU work inside the Job Pod; no Kubernetes or remote executor client."""
import errno
import hashlib
import json
import os
from pathlib import Path
import signal
import sys
import time


def security():
    readonly = bool(os.statvfs('/').f_flag & os.ST_RDONLY)
    write_denied = False
    try:
        Path('/agentbox-write-test').write_text('unexpected')
    except OSError as error:
        write_denied = error.errno in (errno.EROFS, errno.EACCES)
    token = Path('/var/run/secrets/kubernetes.io/serviceaccount/token').exists()
    if os.getuid() != 1000 or not readonly or not write_denied or token:
        raise RuntimeError('batch security contract failed')
    return {'uid': os.getuid(), 'readonly_root': readonly, 'serviceaccount_token': token}


def main():
    mode = sys.argv[1]
    if mode not in ('success', 'failure', 'cancel', 'deadline'):
        raise ValueError('unknown batch probe')
    boundary = security()
    run_id, pod_uid = os.environ['BATCH_RUN'], os.environ['POD_UID']
    ready = {'phase': 'running', 'run': run_id, 'pod_uid': pod_uid, 'mode': mode, **boundary}
    # Ignore TERM in a real parent/child computation to exercise final KILL cleanup.
    if mode == 'cancel':
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        child = os.fork()
        if child == 0:
            until = time.monotonic() + 120
            while time.monotonic() < until:
                hashlib.sha256(b'agentbox-child' * 4096).digest()
            os._exit(0)
        ready['child_pid'] = child
    print(json.dumps(ready), flush=True)
    # Keep real CPU work alive until the host has captured runtime/cgroup evidence.
    # The activation file is fixture coordination, not the Job's business input.
    until = time.monotonic() + 120
    iterations = 0
    while time.monotonic() < until:
        hashlib.sha256(b'agentbox-batch' * 4096).digest()
        iterations += 1
        if mode in ('success', 'failure') and Path('/work/go').exists():
            break
    else:
        raise TimeoutError('batch activation or cancellation deadline')
    if mode == 'failure':
        print(json.dumps({'phase': 'failed', 'run': run_id, 'pod_uid': pod_uid, 'exit_code': 23}), flush=True)
        return 23
    result = {'phase': 'complete', 'run': run_id, 'pod_uid': pod_uid,
              'result': sum(i * i for i in range(1, 1001)), 'iterations': iterations}
    Path('/work/result.json').write_text(json.dumps(result))
    print(json.dumps(result), flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
