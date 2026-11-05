"""Shared API egress expansion for products and acceptance, with explicit endpoints."""

import ipaddress
import json


def endpoint_rules(endpoints):
    if not isinstance(endpoints, list) or not 1 <= len(endpoints) <= 32:
        raise ValueError('API server endpoints must be a bounded non-empty list')
    rules, seen = [], set()
    for endpoint in endpoints:
        if not isinstance(endpoint, dict) or set(endpoint) != {'address', 'port'}:
            raise ValueError('API server endpoint requires address and port')
        address, port = ipaddress.ip_address(endpoint['address']), endpoint['port']
        if address.is_unspecified or address.is_multicast or type(port) is not int or not 1 <= port <= 65535:
            raise ValueError('invalid API server endpoint')
        key = (str(address), port)
        if key not in seen:
            rules.append({'to': [{'ipBlock': {'cidr': str(address)+'/'+str(address.max_prefixlen)}}],
                          'ports': [{'protocol': 'TCP', 'port': port}]})
            seen.add(key)
    return rules


def discover_api_endpoints(kubectl):
    service = json.loads(kubectl('-n', 'default', 'get', 'service', 'kubernetes', '-o', 'json'))
    slices = json.loads(kubectl('-n', 'default', 'get', 'endpointslices', '-l',
        'kubernetes.io/service-name=kubernetes', '-o', 'json'))['items']
    endpoints = [{'address': service['spec']['clusterIP'], 'port': 443}]
    for item in slices:
        endpoints += [{'address': address, 'port': port['port']} for endpoint in item['endpoints']
                      for address in endpoint['addresses'] for port in item['ports']]
    endpoint_rules(endpoints)
    return endpoints


def add_api_egress(policy, endpoints):
    policy['spec']['egress'].extend(endpoint_rules(endpoints))
    return policy
