#!/usr/bin/env python3
"""Inspect components or acceptance fixtures, without contacting a cluster."""
import argparse
from pathlib import Path
import sys
import yaml
from lib.manifests import ROOT,ManifestError,canonical,normalized,objects,profiles,render,render_component,render_fixture


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    choice=parser.add_mutually_exclusive_group(required=True)
    choice.add_argument('--component',choices=['api','cleanup','agent-sandbox','gvisor','batch','batch-api'])
    choice.add_argument('--fixture',choices=['smoke','gvisor','network','sessions','batch'])
    choice.add_argument('--all',action='store_true',help='check every orchestration selection')
    parser.add_argument('--environment',choices=['base','lab'],default='lab')
    parser.add_argument('--set',action='append',default=[],metavar='KEY=VALUE',help='non-secret runtime preview value')
    parser.add_argument('--check',action='store_true')
    parser.add_argument('--output',type=Path,help='new output YAML file (component/fixture only)')
    args=parser.parse_args(argv);params={}
    if args.all and args.output:parser.error('--output requires a component or fixture')
    for value in args.set:
        key,separator,scalar=value.partition('=')
        if not separator or key in params:parser.error('parameters require unique KEY=VALUE entries')
        params[key]=scalar
    config=ROOT/'deploy'
    if args.all:
        values={name:render(config,name,params or None,strict=False) for name in profiles(config)}
    elif args.component:values={args.component:render_component(config,args.component,params or None,args.environment)}
    else:values={args.fixture:render_fixture(config,args.fixture,params or None)}
    if args.check:
        print(yaml.safe_dump({'checks':{name:{'objects':len(objects(value)),'semantic_sha256':canonical(normalized(value))} for name,value in values.items()}},sort_keys=False),end='')
    else:
        text=yaml.safe_dump_all([obj for value in values.values() for obj in objects(value)],sort_keys=False,allow_unicode=True)
        if args.output:
            with args.output.open('x') as stream:stream.write(text)
        else:print(text,end='')
    return 0

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ManifestError,OSError) as error:print(str(error),file=sys.stderr);raise SystemExit(1)
