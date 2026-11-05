#!/usr/bin/env python3
"""Prepare the fixed image dataset and wheel."""
import argparse
import json
from lib.product.dataset import prepare

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--download',action='store_true')
    args=parser.parse_args()
    cache, contract=prepare(args.download)
    print(json.dumps({'cache':str(cache),'dataset_id':contract['dataset_id'],'dataset_sha256':contract['dataset_sha256'],'outputs':len(contract['outputs'])}))

if __name__=='__main__': main()
