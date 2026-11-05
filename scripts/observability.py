#!/usr/bin/env python3
"""Manage the owned loopback observability stack and explicit read-only scope."""
import argparse
import json
from pathlib import Path
import sys
import uuid

from lib.core.paths import ROOT
from lib.core.state import utc, file_hash
from lib.core.runner import Runner
from lib.platform.lab import Lab
from lib.observability.stack import Observability


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('up', 'down', 'watch', 'stop-watch'))
    parser.add_argument('--session-namespace')
    parser.add_argument('--batch-namespace')
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    if args.action == 'watch' and not (args.session_namespace or args.batch_namespace):
        parser.error('watch requires an explicit namespace scope')
    output = args.output or ROOT / 'artifacts' / ('observability-' + args.action + '-' + uuid.uuid4().hex[:12])
    output.mkdir(parents=True, exist_ok=False)
    runner = Runner(output)
    report = {'schema_version': 1, 'started_at': utc(), 'action': args.action, 'ok': False,
              'source_sha256': {str(p.relative_to(ROOT)): file_hash(p) for p in
                [ROOT / 'scripts/observability.py', ROOT / 'scripts/lib/observability/stack.py',
                 *sorted((ROOT / 'deploy/components/observability').glob('*.yaml'))]}}
    try:
        lab = Lab(ROOT, runner)
        stack = Observability(lab)
        if args.action == 'up':
            report['result'] = stack.up()
        elif args.action == 'down':
            report['result'] = stack.down()
        elif args.action == 'stop-watch':
            with lab.operation_lock():
                stack.stop_observer()
            report['result'] = {'stopped': True}
        else:
            scope = {key: value for key, value in {'session': args.session_namespace, 'batch': args.batch_namespace}.items() if value}
            report['result'] = stack.watch(scope)
        report['ok'] = True
    except Exception as error:
        report.update(reason=getattr(error, 'reason', 'observation_failed'), detail=str(error))
    report.update(finished_at=utc(), commands=runner.commands)
    Lab.write_json(output / 'report.json', report)
    print(json.dumps({'ok': report['ok'], 'reason': report.get('reason'), 'report': str(output / 'report.json')}))
    return 0 if report['ok'] else 1


if __name__ == '__main__':
    sys.exit(main())
