"""Frozen R/B plans and independent ledger accounting; no cluster dependencies."""
from collections import Counter
import hashlib
import json
import math
import random


def integer(value, low, high):
    return type(value) is int and low <= value <= high


def validate(config):
    if config.get('schema_version') != 1:
        raise ValueError('unsupported baseline schema')
    bounds = {'repetitions': (1, 5), 'seed': (0, 2**32-1), 'window_ms': (5000, 120000),
              'drain_seconds': (1, 180), 'session_count': (2, 2), 'pool_replicas': (2, 2),
              'session_pod_budget': (4, 8), 'deadline_ms': (1, 60000),
              'max_lag_ms': (1, 5000), 'max_inflight': (1, 16)}
    for name, (low, high) in bounds.items():
        if not integer(config.get(name), low, high):
            raise ValueError('invalid baseline bound: ' + name)
    if config['session_pod_budget'] < config['session_count'] + config['pool_replicas']:
        raise ValueError('session budget must include used and warm members')
    ratio = config.get('minimum_timely_ratio')
    if type(ratio) not in (int, float) or not 0 < ratio <= 1:
        raise ValueError('invalid timely success target')
    if config.get('work') != 'short' or config.get('groups') != {'R': 0, 'B1': 1, 'B2': 2}:
        raise ValueError('baseline requires frozen work and R/B1/B2 groups')
    profiles = config.get('profiles', {})
    if not isinstance(profiles, dict) or not 1 <= len(profiles) <= 3:
        raise ValueError('invalid baseline profiles')
    for name, phases in profiles.items():
        if name not in ('steady-low', 'steady-high', 'step') or not isinstance(phases, list) or not phases or len(phases) > 3:
            raise ValueError('invalid profile phases')
        if any(not isinstance(p, dict) or not integer(p.get('duration_ms'), 1, 120000) or
               not integer(p.get('interval_ms'), 100, 120000) for p in phases):
            raise ValueError('invalid profile duration or interval')
        if sum(p['duration_ms'] for p in phases) != config['window_ms']:
            raise ValueError('profile duration must equal measurement window')
    batch = config.get('batch', {})
    arrivals = batch.get('arrivals_ms', [])
    if not integer(batch.get('passes'), 1, 4096) or not isinstance(arrivals, list) or not 1 <= len(arrivals) <= 4:
        raise ValueError('invalid batch plan')
    if any(not integer(at, 0, config['window_ms']-1) for at in arrivals) or arrivals != sorted(set(arrivals)):
        raise ValueError('invalid batch arrivals')
    # Durable records are intentionally not evicted by the current product.
    if config['repetitions'] * len(profiles) * 2 * len(arrivals) > 120:
        raise ValueError('baseline exceeds durable batch record headroom')
    return config


def requests(config, profile):
    arrivals, start = [], 0
    for phase in config['profiles'][profile]:
        arrivals.extend(range(start, start + phase['duration_ms'], phase['interval_ms']))
        start += phase['duration_ms']
    return [{'id': 'command-' + str(i), 'at_ms': at, 'target': i % config['session_count']}
            for i, at in enumerate(arrivals)]


def eligible_profiles(config, reference):
    if reference.get('stage') != 'calibration' or reference.get('config_sha256') != digest(config) or not reference.get('cleanup_complete'):
        raise ValueError('comparison requires matching frozen R calibration')
    eligible = []
    for profile in config['profiles']:
        cells = [c for c in reference.get('cells', []) if c['group'] == 'R' and c['profile'] == profile]
        if len(cells) == config['repetitions'] and len({c['repetition'] for c in cells}) == config['repetitions'] and all(
                c['measurement_valid'] and c['session']['quality_target_met'] for c in cells):
            eligible.append(profile)
    if not eligible:
        raise ValueError('no R profile has sufficient valid rounds meeting the frozen quality target')
    return eligible


def plan(config, stage='calibration', reference=None):
    validate(config)
    rng = random.Random(config['seed'])
    cells = []
    if stage == 'calibration':
        # Start with the lowest peak arrival rate. A high-load observation
        # failure must not prevent collecting the lower-load R reference first.
        profiles = sorted(config['profiles'], key=lambda p: min(v['interval_ms'] for v in config['profiles'][p]), reverse=True)
        for profile in profiles:
            for repetition in range(1, config['repetitions']+1):
                cells.append({'id': f'r{repetition}-{profile}-R', 'cohort': f'cal-{profile}-{repetition}',
                    'repetition': repetition, 'profile': profile, 'group': 'R', 'batch_slots': 0,
                    'requests': requests(config, profile)})
        return cells
    if stage != 'comparison' or reference is None:
        raise ValueError('invalid baseline stage or missing R reference')
    allowed = eligible_profiles(config, reference)
    for repetition in range(config['repetitions']):
        profiles = list(allowed)
        rng.shuffle(profiles)
        for profile in profiles:
            groups = list(config['groups'])
            rng.shuffle(groups)
            for group in groups:
                cells.append({'id': f'r{repetition+1}-{profile}-{group}',
                              'cohort': f'comparison-{repetition+1}',
                              'repetition': repetition+1, 'profile': profile, 'group': group,
                              'batch_slots': config['groups'][group],
                              'requests': requests(config, profile)})
    return cells


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def percentile(values, percent):
    return sorted(values)[max(0, math.ceil(len(values)*percent/100)-1)] if values else None


def session_summary(trace, events, config):
    expected = {r['id']: r for r in trace['requests']}
    actual = {e['id']: e for e in events}
    if len(actual) != len(events) or set(actual) != set(expected):
        raise ValueError('unbalanced or duplicate session ledger')
    for key, event in actual.items():
        r = expected[key]
        if event['planned_ms'] != r['at_ms'] or event['session_id'] != trace['targets'][r['target']]['id']:
            raise ValueError('ledger target or planned time differs')
    sent = [e for e in events if e['sent_ms'] >= 0]
    correct = [e for e in sent if e['outcome'] == 'correct']
    timely = [e for e in correct if e['finished_ms']-e['planned_ms'] <= config['deadline_ms']]
    valid = len(sent) == len(events) and all(0 <= e['sent_ms']-e['planned_ms'] <= config['max_lag_ms'] for e in sent)
    result = {'planned': len(events), 'sent': len(sent), 'dropped': len(events)-len(sent),
              'outcomes': dict(Counter(e['outcome'] for e in events)),
              'correct': len(correct), 'timely_correct': len(timely),
              'timely_ratio': len(timely)/len(events), 'measurement_valid': valid,
              'quality_target_met': valid and len(timely)/len(events) >= config['minimum_timely_ratio'],
              'offered_per_second': len(events)/(config['window_ms']/1000),
              'timely_goodput_per_second': len(timely)/(config['window_ms']/1000)}
    for name, selected in (('all_sent', sent), ('correct', correct)):
        latencies = [e['finished_ms']-e['sent_ms'] for e in selected]
        result[name + '_latency_ms'] = {f'p{p}': percentile(latencies, p) for p in (50, 95, 99)}
    return result


def batch_summary(entries, window_seconds, passes):
    verified = [v for v in entries if v.get('verified_at') is not None]
    within = [v for v in verified if v['verified_at'] <= window_seconds]
    waits = [v['queue_wait_seconds'] for v in entries if v.get('queue_wait_seconds') is not None]
    ages = [window_seconds-v['planned_ms']/1000 for v in entries if v not in within]
    # Verified completion is an observed upper bound, not an invented exact finish time.
    return {'planned': len(entries), 'verified': len(verified), 'verified_within_window': len(within),
            'verified_passes_within_window': len(within)*passes,
            'verified_passes_per_second': len(within)*passes/window_seconds,
            'on_time_ratio': len(within)/len(entries) if entries else None,
            'backlog_at_window': len(entries)-len(within),
            'max_unfulfilled_age_seconds_at_window': max(ages, default=None),
            'queue_wait_seconds': {'p50': percentile(waits, 50), 'max': max(waits, default=None)},
            'submission_outcomes': dict(Counter(v.get('outcome', 'unrecorded') for v in entries)),
            'completion_upper_bound_seconds': max((v['verified_at'] for v in verified), default=None)
                if len(verified) == len(entries) and entries else None,
            'unfinished': len(entries)-len(verified),
            'drain_verified': len(verified)-len(within)}


def batch_stages(snapshots, entries):
    """Retain upstream timestamps and attempts; observation delays are explicit."""
    by_uid = {entry['batch_id'].split('.', 1)[1]: entry for entry in entries if entry.get('batch_id')}
    for snapshot in snapshots:
        for resource in ('jobs', 'pods', 'workloads.kueue.x-k8s.io'):
            for obj in snapshot.get(resource, []):
                meta = obj['metadata']
                uid = meta['uid'] if resource == 'jobs' else next((r['uid'] for r in meta.get('ownerReferences', []) if r.get('kind') == 'Job' and r.get('controller')), None)
                entry = by_uid.get(uid)
                if entry is None:
                    continue
                stages = entry.setdefault('stages', {})
                if resource == 'jobs':
                    stages.setdefault('submitted_at', meta['creationTimestamp'])
                    for key in ('startTime', 'completionTime'):
                        if obj.get('status', {}).get(key):
                            stages[key] = obj['status'][key]
                elif resource == 'workloads.kueue.x-k8s.io':
                    entry.setdefault('workload_uid', meta['uid'])
                    if entry['workload_uid'] != meta['uid']:
                        raise ValueError('batch Workload identity replaced')
                    for condition in obj.get('status', {}).get('conditions', []):
                        if condition['type'] in ('QuotaReserved', 'Admitted', 'Finished') and condition['status'] == 'True':
                            stages[condition['type']] = condition['lastTransitionTime']
                else:
                    attempt = entry.setdefault('attempts', {}).setdefault(meta['uid'], {'name': meta['name']})
                    attempt.update(phase=obj.get('status', {}).get('phase'), status=obj.get('status', {}))
                    attempt.setdefault('first_observed_seconds', snapshot['elapsed_seconds'])
    for entry in entries:
        stages = entry.get('stages', {})
        def timestamp(key):
            from datetime import datetime
            return datetime.fromisoformat(stages[key].replace('Z', '+00:00')).timestamp() if stages.get(key) else None
        submitted, admitted, started, completed = (timestamp(k) for k in ('submitted_at', 'Admitted', 'startTime', 'completionTime'))
        entry['queue_wait_seconds'] = admitted-submitted if admitted is not None and submitted is not None else None
        entry['job_run_seconds'] = completed-started if completed is not None and started is not None else None
    return entries


def aggregate(cells):
    """Keep independent rounds visible; never average Pod or round percentiles."""
    groups = {}
    for cell in cells:
        key = cell['profile'] + '/' + cell['group']
        groups.setdefault(key, []).append(cell)
    result = {}
    for key, values in groups.items():
        timely = [c['session']['timely_ratio'] for c in values]
        result[key] = {'rounds': len(values),
                       'valid_rounds': sum(c['measurement_valid'] for c in values),
                       'quality_met_rounds': sum(c['session']['quality_target_met'] for c in values),
                       'timely_ratio_by_round': timely,
                       'median_timely_ratio': percentile(timely, 50),
                       'batch_verified_passes_per_second_by_round': [c['batch']['verified_passes_per_second'] for c in values]}
    return result


def observations(series, start, end):
    """Sample coverage and integrals stay separate from business correctness.

    Adjacent samples > 10s apart are gaps, never interpolated as zero usage.
    CRI and cAdvisor are alternative views; parents and children are not summed.
    """
    def samples(name):
        return series.get(name, {}).get('data', {}).get('result', [])

    def coverage(values):
        points = [(float(t), float(v)) for t, v in values if start <= float(t) <= end]
        if not points or any(not math.isfinite(v) for _, v in points):
            return False
        return points[0][0]-start <= 10 and end-points[-1][0] <= 10 and all(
            0 < b[0]-a[0] <= 10 for a, b in zip(points, points[1:]))

    issues = []
    for domain in ('session', 'batch'):
        matches = [s for s in samples('agentbox_inventory_valid') if s['metric'].get('domain') == domain]
        if not matches or not all(coverage(s['values']) and all(float(v) == 1 for _, v in s['values']) for s in matches):
            issues.append(domain+'_inventory_unknown')
        stamps = [s for s in samples('agentbox_inventory_timestamp_seconds') if s['metric'].get('domain') == domain]
        if not stamps or not all(coverage(s['values']) and all(0 <= float(t)-float(v) <= 15 for t, v in s['values']) for s in stamps):
            issues.append(domain+'_inventory_stale')
    for job, instance in (('agentbox-api', 'session'), ('agentbox-api', 'batch'), ('observer', 'namespace-observer')):
        matches = [s for s in samples('up') if s['metric'].get('job') == job and s['metric'].get('instance') == instance]
        if not matches or not all(coverage(s['values']) and all(float(v) == 1 for _, v in s['values']) for s in matches):
            issues.append(instance+'_scrape_missing')
    costs = []
    for s in samples('agentbox_runtime_resource'):
        metric = s['metric']
        if metric.get('source') != 'cri' or metric.get('resource') not in ('cpu_cores', 'working_set_bytes'):
            continue
        points = [(float(t), float(v)) for t, v in s['values'] if start <= float(t) <= end]
        integral = sum((b[0]-a[0])*(a[1]+b[1])/2 for a, b in zip(points, points[1:])
                       if 0 < b[0]-a[0] <= 10 and all(math.isfinite(v) for v in (a[1], b[1])))
        costs.append({'labels': metric, 'observed_integral': integral if len(points) >= 2 else None,
                      'unit': 'cpu_seconds' if metric['resource'] == 'cpu_cores' else 'byte_seconds',
                      'sampled_seconds': sum(b[0]-a[0] for a, b in zip(points, points[1:]) if 0 < b[0]-a[0] <= 10),
                      'covers_window': coverage(s['values'])})
    return {'inventory_and_scrapes_valid': not issues, 'issues': issues, 'container_costs': costs,
            'scope': 'CRI leaf containers only; gaps, outside-Pod runtime and unsampled short attempts are unknown; do not sum alternative sources'}
