#!/usr/bin/env python3
"""Inspect or remove the fixed, quiescent legacy systemd installation only."""
import argparse
import contextlib
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile


def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def atomic_report(path, value):
    fd, tmp = tempfile.mkstemp(prefix='.agentbox-retirement-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, indent=2, sort_keys=True); stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
        os.replace(tmp, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try: os.fsync(directory)
        finally: os.close(directory)
    finally:
        if os.path.exists(tmp): os.unlink(tmp)


class Retirement:
    def __init__(self, root=Path('/'), command=None):
        self.root = Path(root).absolute()
        self.live = self.root == Path('/')
        self.command = command or self.run
        self.app = self.root/'opt/agentbox'
        self.state = self.root/'var/lib/agentbox'
        self.unit = self.root/'etc/systemd/system/agentbox.service'
        self.config = self.root/'etc/agentbox.env'

    @staticmethod
    def run(args):
        return subprocess.run(args, capture_output=True, text=True, timeout=30, check=True).stdout

    def safe_path(self, path):
        for value in (path, *path.parents):
            if value.is_symlink(): raise ValueError('administrative path is a symlink')
            if value == self.root: break
        if self.live and path.exists() and (path.stat().st_uid != 0 or path.stat().st_mode & 0o022):
            raise ValueError('administrative path must be root-owned and not writable by others')

    def inspect(self):
        for path in (self.app, self.state, self.unit, self.config): self.safe_path(path)
        manifest_path = self.app/'INSTALL.json'
        self.safe_path(manifest_path)
        manifest = json.loads(manifest_path.read_text())
        if manifest.get('format') != 1 or manifest.get('platform') not in ('kvm', 'systrap'):
            raise ValueError('unrecognized legacy installation manifest')
        binary = self.app/'bin/agentboxd'; runsc = self.app/'.local/gvisor/runsc'
        if digest(binary) != manifest['binary_sha256'] or digest(runsc) != manifest['runsc_sha256']:
            raise ValueError('legacy installation checksum mismatch')
        if set(p.name for p in self.app.iterdir()) - {'INSTALL.json', 'bin', '.local', 'previous'}:
            raise ValueError('unrecognized application contents; preserve for manual inventory')
        if set(p.name for p in (self.app/'.local').iterdir()) - {'gvisor','rootfs','rootfs-image.json'}:
            raise ValueError('application runtime directory contains shared/new components')
        previous = self.app/'previous'
        if previous.exists() and (previous.is_symlink() or set(p.name for p in previous.iterdir()) - {'agentboxd','agentbox.service','INSTALL.json'}):
            raise ValueError('unrecognized rollback backup contents')
        if set(p.name for p in (self.app/'bin').iterdir()) != {'agentboxd'}:
            raise ValueError('application contains another component; do not remove shared files')
        if (self.app/'GATEWAY_INSTALL.json').exists(): raise ValueError('paired Gateway must be inventoried first')
        if self.unit.with_name('agentbox.service.d').exists():
            if any(self.unit.with_name('agentbox.service.d').iterdir()): raise ValueError('unit drop-ins need separate inventory')
        text = self.unit.read_text()
        starts = [line.split('=', 1)[1] for line in text.splitlines() if line.startswith('ExecStart=')]
        if len(starts) != 1: raise ValueError('unexpected legacy unit command')
        args = shlex.split(starts[0])
        expected = {'-runsc':'/opt/agentbox/.local/gvisor/runsc','-rootfs':'/opt/agentbox/.local/rootfs','-state-dir':'/var/lib/agentbox'}
        if not args or args[0] != '/opt/agentbox/bin/agentboxd': raise ValueError('unit belongs to another executable')
        for key, value in expected.items():
            if key not in args or args.index(key)+1 >= len(args) or args[args.index(key)+1] != value:
                raise ValueError('unit uses a nonstandard resource path')
        children = set(p.name for p in self.state.iterdir()) if self.state.exists() else set()
        unknown = sorted(children - {'bundles','runsc','owner.json','daemon.lock'})
        if unknown: raise ValueError('unrecognized state entries: '+', '.join(unknown))
        for name in ('bundles','runsc'):
            directory = self.state/name; self.safe_path(directory)
            if directory.exists():
                if not directory.is_dir(): raise ValueError('legacy state entry is not a directory: '+name)
                remaining = sorted(p.name for p in directory.iterdir())
                if remaining: raise ValueError('unreconciled '+name+' entries: '+', '.join(remaining[:20]))
        owner_path = self.state/'owner.json'; self.safe_path(owner_path)
        if owner_path.exists():
            owner = json.loads(owner_path.read_text())
            if owner != {'platform':manifest['platform'], 'rootfs':'/opt/agentbox/.local/rootfs','cgroup_root':'/sys/fs/cgroup/system.slice/agentbox.service'}:
                raise ValueError('state ownership does not match the fixed legacy unit')
        if self.live:
            for line in Path('/proc/self/mountinfo').read_text().splitlines():
                mounted = re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), line.split()[4])
                if any(mounted == str(p) or mounted.startswith(str(p)+'/') for p in (self.app,self.state)):
                    raise ValueError('mounted resources remain under legacy paths')
            processes = self.command(['ps','-eo','pid=,comm='])
            if any(line.split()[-1] in ('agentboxd','agentbox-gatewa','agentbox-gateway') for line in processes.splitlines() if line.split()):
                raise ValueError('legacy processes are still running; drain before retirement')
            status = dict(line.split('=',1) for line in self.command(['systemctl','show','agentbox.service','--property=ActiveState,ExecMainPID']).splitlines() if '=' in line)
            if status.get('ActiveState') not in ('inactive','failed') or status.get('ExecMainPID') != '0':
                raise ValueError('legacy service is not quiescent')
            events = self.root/'sys/fs/cgroup/system.slice/agentbox.service/cgroup.events'
            if events.exists() and not re.search(r'^populated 0$', events.read_text(), re.M): raise ValueError('legacy cgroup still contains processes')
        return {'can_retire':True, 'manifest':manifest, 'unit_sha256':digest(self.unit), 'bundles_remaining':0,
                'paths':[str(p) for p in (self.unit,self.config,self.app,self.state)],
                'scope':'fixed legacy installation only; workspace platform caches, Kubernetes and user artifacts are excluded'}

    @contextlib.contextmanager
    def locks(self):
        paths = [self.root/'run/lock/agentbox-deploy.lock', self.state/'daemon.lock']
        handles = []
        try:
            for path in paths:
                self.safe_path(path)
                if not path.parent.is_dir(): continue
                fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600); handles.append(fd)
                info = os.fstat(fd)
                if self.live and (info.st_uid != 0 or info.st_mode & 0o077): raise ValueError('administrative lock is not private/root-owned')
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            yield
        finally:
            for fd in reversed(handles): os.close(fd)

    def apply(self, report, save):
        with self.locks():
            current = self.inspect()
            if current['unit_sha256'] != report['inspection']['unit_sha256'] or current['manifest'] != report['inspection']['manifest']:
                raise ValueError('installation changed after inspection')
            backup_parent = self.root/'var/backups'
            self.safe_path(backup_parent); backup_parent.mkdir(exist_ok=True)
            backup = Path(tempfile.mkdtemp(prefix='agentbox-retired-', dir=backup_parent)); backup.chmod(0o700)
            for source,name in ((self.unit,'agentbox.service'),(self.config,'agentbox.env'),(self.app/'INSTALL.json','INSTALL.json'),(self.state/'owner.json','owner.json')):
                if source.exists():
                    target=backup/name;shutil.copyfile(source,target);target.chmod(0o600)
                    with target.open('rb') as stream: os.fsync(stream.fileno())
            for directory in (backup, backup_parent):
                fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
                try: os.fsync(fd)
                finally: os.close(fd)
            report['backup'] = str(backup)
            report['action'] = 'remove_legacy_installation'; report['outcome'] = 'planned'; save(report)
            if self.live: self.command(['systemctl','disable','agentbox.service'])
            self.unit.unlink(); self.config.unlink(missing_ok=True)
            shutil.rmtree(self.app)
            if self.state.exists(): shutil.rmtree(self.state)
            if self.live: self.command(['systemctl','daemon-reload'])
            report['outcome']='removed';save(report)


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply',action='store_true',help='remove only a verified, inactive legacy installation')
    parser.add_argument('--report',type=Path,required=True,help='new private report path; never contains credentials')
    args=parser.parse_args(argv)
    if os.geteuid()!=0: parser.error('requires sudo to verify private legacy state; default mode is inspection only')
    fd=os.open(args.report,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600);os.close(fd)
    record={'schema_version':1,'started_at':datetime.now(timezone.utc).isoformat(),'apply':args.apply,'outcome':'inspection_pending'}
    def save(value):
        atomic_report(args.report,value)
        # Only the metadata report is returned to the invoking administrator;
        # the backup containing credentials stays root-only under /var/backups.
        if 'SUDO_UID' in os.environ: os.chown(args.report,int(os.environ['SUDO_UID']),-1)
    try:
        retirement=Retirement();record['inspection']=retirement.inspect();record['outcome']='inspected';save(record)
        if args.apply: retirement.apply(record,save)
    except (OSError,ValueError,subprocess.SubprocessError) as error:
        record['outcome']='incomplete' if record.get('action') else 'blocked'
        record['error']=str(error)
        print(json.dumps({'outcome':record['outcome'],'error':record['error'],'report':str(args.report)}))
        return 1
    finally:
        record['finished_at']=datetime.now(timezone.utc).isoformat();save(record)
    print(json.dumps({'outcome':record['outcome'],'report':str(args.report),'backup':record.get('backup')}))
    return 0

if __name__=='__main__':raise SystemExit(main())
