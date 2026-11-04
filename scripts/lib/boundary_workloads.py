"""Frozen, bounded Python workloads and independent expected results."""
import hashlib


def compile_workload(kind, marker):
    prefix = "from pathlib import Path\nassert Path('marker').read_text()==" + repr(marker) + "\n"
    if kind in ('short', 'long'):
        n = 10000000 if kind == 'long' else 1000
        return prefix + 'print(sum(i*i for i in range(' + str(n) + ')))', str((n-1)*n*(2*n-1)//6)+'\n', 0
    if kind == 'memory':
        return prefix + "b=bytearray(32*1024*1024)\nb[0]=3;b[-1]=7\nprint(len(b),b[0]+b[-1])", '33554432 10\n', 0
    if kind == 'files':
        data = b'0123456789abcdef' * 8192
        return prefix + "import hashlib\nb=b'0123456789abcdef'*8192\nPath('payload').write_bytes(b)\nr=Path('payload').read_bytes()\nassert r==b\nprint(hashlib.sha256(r).hexdigest())", hashlib.sha256(data).hexdigest()+'\n', 0
    if kind == 'output':
        return prefix + "print('x'*8192)", 'x'*8192+'\n', 0
    if kind == 'failure':
        return prefix + "print('expected-program-failure')\nraise SystemExit(23)", 'expected-program-failure\n', 23
    raise ValueError('unknown workload: ' + kind)
