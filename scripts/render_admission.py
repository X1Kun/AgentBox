#!/usr/bin/env python3
"""Render a separate opt-in AdmissionCheck deployment; does not apply or attach it."""
import argparse
from pathlib import Path
from lib.core.paths import ROOT
from lib.manifests import read_yaml
from lib.product.admission import compose
from lib.product.deployment import write_bundle

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inputs',type=Path,required=True,help='explicit policy/image/controller namespace/IP endpoints YAML')
    parser.add_argument('--output',type=Path,required=True,help='new private manifest path')
    parser.add_argument('--prometheus-ca-file',type=Path,help='optional bounded public CA PEM, embedded read-only with the policy')
    args=parser.parse_args()
    value=read_yaml(args.inputs)
    if set(value)!={'policy','image','controller_namespace','api_server_endpoints','prometheus_endpoints'}:
        parser.error('inputs require policy, image, controller_namespace and explicit API/Prometheus endpoints')
    ca=None
    if args.prometheus_ca_file:
        with args.prometheus_ca_file.open('r') as stream:ca=stream.read((1<<20)+1)
    write_bundle(args.output,compose(ROOT/'deploy',value['policy'],value['image'],value['controller_namespace'],
                                    value['api_server_endpoints'],value['prometheus_endpoints'],ca))

if __name__=='__main__':main()
