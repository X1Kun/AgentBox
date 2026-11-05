"""Trusted identity-reporting HTTP endpoints used only by network acceptance."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import sys
import threading
import time

assert os.getuid() == 1000
assert not os.path.exists('/var/run/secrets/kubernetes.io/serviceaccount/token')


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        value = json.dumps({'pod_uid': os.environ['POD_UID'], 'run': os.environ['NET_RUN'],
                            'port': self.server.server_port}).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(value)))
        self.end_headers()
        self.wfile.write(value)

    def log_message(self, *args):
        pass


for port in [8000, 8001, 8080, 8081]:
    server = ThreadingHTTPServer(('0.0.0.0', port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
print(json.dumps({'ready': True, 'uid': os.getuid()}), flush=True)
while True:
    time.sleep(60)
