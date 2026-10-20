#!/usr/bin/env python3
"""Offline analysis/plotting or recording an existing service; no implicit load runs."""

import argparse
import os
from pathlib import Path
import sys

sys.dont_write_bytecode = True
os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts/lib"))


def main():
    if len(sys.argv) < 2 or sys.argv[1] in ("help", "-h", "--help"):
        print("Usage: python3 scripts/experiment.py {record|analyze|plot} [arguments]")
        return 0
    command = sys.argv.pop(1)
    if command == "record":
        from observe import main as record
        record()
    elif command == "plot":
        from plot_static import main as plot
        plot()
    elif command == "analyze":
        from observation_analysis import analyze
        parser = argparse.ArgumentParser(description="Analyze existing observations without starting workloads")
        parser.add_argument("directory", type=Path)
        parser.add_argument("--output", type=Path, required=True)
        parser.add_argument("--no-plots", action="store_true")
        args = parser.parse_args()
        os.environ.setdefault("MPLCONFIGDIR", str(ROOT / ".local/matplotlib-cache"))
        result = analyze(args.directory, args.output, plots=not args.no_plots)
        print(f"Analyzed {len(result['runs'])} runs; new load: false; output: {args.output}")
    else:
        print("Unknown experiment command", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
