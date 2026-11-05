"""Reentrant per-instance ownership lock with an OS-level cross-process guard."""

from contextlib import contextmanager
import fcntl
from pathlib import Path
import threading
from lib.core.errors import LabError

class OperationLock:
    def __init__(self, path):
        self.path = Path(path)
        self.owner = None

    @contextmanager
    def hold(self):
        owner = threading.get_ident()
        if self.owner == owner:
            yield
            return
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        if self.path.parent.is_symlink():
            raise LabError('invalid_state', 'state directory must not be a symlink')
        with self.path.open('a+') as stream:
            try:
                fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise LabError('operation_in_progress', 'another platform operation holds the lock') from error
            self.owner = owner
            try:
                yield
            finally:
                self.owner = None
