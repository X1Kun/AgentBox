#!/usr/bin/env python3
"""Extract additional metrics from existing artifacts without running workloads."""
import argparse
import os
from pathlib import Path
from observation_analysis import analyze

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument("directory",type=Path)
parser.add_argument("--output",type=Path,required=True,help="new directory; existing results are never overwritten")
parser.add_argument("--no-plots",action="store_true")
args=parser.parse_args()
os.environ.setdefault("MPLCONFIGDIR",str(Path(__file__).resolve().parents[1]/".local/matplotlib-cache"))
result=analyze(args.directory,args.output,plots=not args.no_plots)
print(f"Analyzed {len(result['runs'])} runs; new load: false; output: {args.output}")
