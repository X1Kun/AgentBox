#!/usr/bin/env python3
"""Deploy AgentBox: base, gateway, or smoke; pass --help after the subcommand."""

import os
from pathlib import Path
import runpy
import sys

sys.dont_write_bytecode = True
os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
LIBRARY = Path(__file__).resolve().parent / "lib"


def main():
    commands = {"base": "systemd_deploy.py", "gateway": "gateway_deploy.py", "smoke": "smoke_host.py"}
    if len(sys.argv) < 2 or sys.argv[1] in ("-h", "--help", "help"):
        print("Usage: python3 scripts/deploy.py {base|gateway|smoke} [arguments]")
        return 0
    command = sys.argv[1]
    if command not in commands:
        print("Unknown deployment command", file=sys.stderr)
        return 2
    sys.path.insert(0, str(LIBRARY))
    sys.argv = [sys.argv[0]+" "+command, *sys.argv[2:]]
    runpy.run_path(str(LIBRARY / commands[command]), run_name="__main__")
    return 0


if __name__ == "__main__":
    sys.exit(main())
