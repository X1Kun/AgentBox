"""Bounded TCP/HTTP and DNS probes; a refusal or invalid response is not a policy denial."""
import json
import os
from pathlib import Path
import socket
import struct
import sys


def receive(stream, count):
    result = b''
    while len(result) < count:
        chunk = stream.recv(count - len(result))
        if not chunk:
            raise ValueError('truncated reply')
        result += chunk
    return result


def skip_name(data, offset):
    for _ in range(128):
        length = data[offset]
        offset += 1
        if length & 0xC0 == 0xC0:
            return offset + 1
        if not length:
            return offset
        if length > 63:
            raise ValueError('invalid DNS label')
        offset += length
    raise ValueError('invalid DNS name')


def dns_addresses(data, request_id):
    response_id, flags, questions, answers, _, _ = struct.unpack('!HHHHHH', data[:12])
    if response_id != request_id or not flags & 0x8000 or flags & 15:
        raise ValueError('invalid DNS response')
    offset = 12
    for _ in range(questions):
        offset = skip_name(data, offset) + 4
    result = []
    for _ in range(answers):
        offset = skip_name(data, offset)
        kind, category, _, size = struct.unpack('!HHIH', data[offset:offset + 10])
        offset += 10
        if kind == category == 1 and size == 4:
            result.append(socket.inet_ntoa(data[offset:offset + 4]))
        offset += size
    return result


def probe(case):
    protocol = case['protocol']
    if protocol.startswith('dns-'):
        resolver = next(line.split()[1] for line in Path('/etc/resolv.conf').read_text().splitlines()
                        if line.startswith('nameserver '))
        query_id = int.from_bytes(os.urandom(2), 'big')
        name = b''.join(bytes([len(label)]) + label.encode() for label in case['name'].split('.')) + b'\0'
        packet = struct.pack('!HHHHHH', query_id, 0x0100, 1, 0, 0, 0) + name + struct.pack('!HH', 1, 1)
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM if protocol == 'dns-udp' else socket.SOCK_STREAM) as sock:
            sock.settimeout(1)
            if protocol == 'dns-udp':
                sock.sendto(packet, (resolver, 53))
                data = sock.recv(4096)
            else:
                sock.connect((resolver, 53))
                sock.sendall(struct.pack('!H', len(packet)) + packet)
                size = struct.unpack('!H', receive(sock, 2))[0]
                if size > 4096:
                    raise ValueError('oversized DNS response')
                data = receive(sock, size)
        return {'kind': 'allowed', 'addresses': dns_addresses(data, query_id), 'resolver': resolver}
    with socket.socket() as sock:
        sock.settimeout(1)
        try:
            sock.connect((case['ip'], case['port']))
        except socket.timeout:
            return {'kind': 'connect_timeout'}
        if protocol == 'tcp':
            return {'kind': 'allowed'}
        sock.sendall(b'GET / HTTP/1.0\r\nHost: network-probe\r\n\r\n')
        data = b''
        try:
            while True:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                data += chunk
                if len(data) > 8192:
                    raise ValueError('oversized HTTP reply')
        except socket.timeout:
            return {'kind': 'read_timeout'}
        return {'kind': 'allowed', 'response': json.loads(data.split(b'\r\n\r\n', 1)[1])}


def run(cases):
    result = []
    for case in cases:
        try:
            value = probe(case)
        except socket.timeout:
            value = {'kind': 'dns_timeout' if case['protocol'].startswith('dns-') else 'unexpected_timeout'}
        except OSError as error:
            value = {'kind': 'socket_error', 'errno': error.errno, 'detail': str(error)}
        except (ValueError, IndexError, struct.error, StopIteration) as error:
            value = {'kind': 'invalid_reply', 'detail': str(error)}
        result.append(dict(id=case['id'], **value))
    return result


if __name__ == '__main__':
    print(json.dumps({'pod_uid': os.environ['POD_UID'], 'run': os.environ['NET_RUN'],
                      'results': run(json.loads(sys.argv[1]))}), flush=True)
