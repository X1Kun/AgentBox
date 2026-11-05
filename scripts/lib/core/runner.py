"""Recorded subprocess execution with bounded output and process-group cancellation."""


import contextlib
import os
from pathlib import Path
import signal
import subprocess
import time

from lib.core.state import utc
from lib.core.errors import CommandError

class Runner:
    def __init__(self, output):
        self.output = output
        self.commands = []

    def run(self, args, timeout=30, input_text=None, announce=True, clear_proxy=False):
        args = [str(x) for x in args]
        if announce:
            print("[platform] " + " ".join(args), flush=True)
        begin = time.monotonic()
        record = {"argv": args, "started_at": utc()}
        self.commands.append(record)
        env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER="docker")
        if clear_proxy:
            # A host loopback proxy is unreachable inside a kind node.
            # Only this child environment changes; never edit host settings.
            for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
                env[key] = ""
            env["NO_PROXY"] = env["no_proxy"] = "*"
            record["node_proxy_forwarding"] = "disabled"
        try:
            proc = subprocess.Popen(args, stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                    env=env, start_new_session=True)
        except OSError as error:
            record.update(reason="tool_unavailable", detail=str(error))
            raise CommandError("tool_unavailable", f"{args[0]}: {error}") from error
        try:
            stdout, stderr = proc.communicate(input_text, timeout=timeout)
        except (subprocess.TimeoutExpired, KeyboardInterrupt) as error:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(proc.pid, signal.SIGKILL)
            stdout, stderr = proc.communicate()
            if isinstance(error, KeyboardInterrupt):
                record["reason"] = "command_interrupted"
                raise
            record.update(reason="command_timeout", timeout_seconds=timeout)
            raise CommandError("command_timeout", f"{Path(args[0]).name} exceeded {timeout}s")
        finally:
            record["elapsed_seconds"] = round(time.monotonic() - begin, 3)
            if "stdout" in locals():
                record.update(exit_code=proc.returncode, stdout=stdout[-65536:], stderr=stderr[-65536:])
        if proc.returncode:
            raise CommandError("command_failed",
                               f"{Path(args[0]).name} exited {proc.returncode}: {stderr[-2000:].strip()}")
        return stdout

