#!/usr/bin/env python3
"""Render one complete lab product; credentials stay in a new private output file."""
import argparse
import json
from pathlib import Path
import sys

from lib.manifests import ROOT, ManifestError, read_yaml
from lib.product.deployment import compose, credentials, validate_inputs, validate_cluster, write_bundle


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True, help='explicit non-secret product inputs YAML')
    parser.add_argument('--credentials-dir', type=Path, required=True, help='private tls.crt, tls.key and principals.json files')
    parser.add_argument('--output', type=Path, required=True, help='new mode-0600 manifest; never stdout')
    parser.add_argument('--kubeconfig', type=Path, help='explicit read-only prerequisite check')
    parser.add_argument('--context', help='explicit context, paired with --kubeconfig')
    args = parser.parse_args(argv)
    if bool(args.kubeconfig) != bool(args.context):
        parser.error('--kubeconfig and --context must be provided together')
    value = validate_inputs(read_yaml(args.config))
    checked = validate_cluster(ROOT/'deploy', value, args.kubeconfig, args.context) if args.kubeconfig else None
    resources = compose(ROOT/'deploy', value, credentials(args.credentials_dir))
    write_bundle(args.output, resources)
    print(json.dumps({'product':value['product'],'output':str(args.output),'objects':len(resources),
                      'cluster_prerequisites':checked,'retention':'apply explicitly; product is retained until operator removal'}))
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (ManifestError,OSError,ValueError) as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(1)
