#!/usr/bin/env python3
"""Run in an offline, read-only image as a non-root user with a fresh HOME."""
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading

lock = json.loads(Path('/opt/artex-tools/security-tools.lock.json').read_text())
home = Path(os.environ['HOME'])
assert os.getuid() != 0, 'exercise the non-root sandbox path'
home.mkdir(parents=True, exist_ok=True)


def run(*args):
    result = subprocess.run(args, text=True, capture_output=True, timeout=90)
    assert result.returncode == 0, (args, result.stdout[-4000:], result.stderr[-4000:])
    return result.stdout + result.stderr


assert lock['vulnx']['version'] in run('vulnx', 'version', '--disable-update-check')
# Several processes with the same fresh HOME must never see partial config.
with ThreadPoolExecutor(max_workers=4) as pool:
    versions = list(pool.map(lambda _: run('nuclei', '-version'), range(4)))
assert all(lock['nuclei']['version'] in result for result in versions)
config = home / '.config/nuclei/.templates-config.json'
data = json.loads(config.read_text())
assert data['nuclei-templates-directory'] == '/opt/nuclei-templates', data
assert data['nuclei-templates-version'] == 'v' + lock['nuclei-templates']['version'], data
templates = run('nuclei', '-tl', '-tags', 'cve', '-silent')
assert 'CVE-' in templates and 'Installing' not in templates, templates[-4000:]
print('Pinned CLI versions, atomic first-run config and offline CVE template listing: OK', flush=True)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ARTEX-local-template-smoke')

    def log_message(self, *_):
        pass


# No exploit or external target: match a fixed string on a local test server.
server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    with tempfile.TemporaryDirectory() as temp:
        template = Path(temp) / 'smoke.yaml'
        template.write_text('''id: artex-local-template-smoke
info:
  name: ARTEX local smoke
  author: artex
  severity: info
http:
  - method: GET
    path:
      - "{{BaseURL}}/"
    matchers:
      - type: word
        words:
          - "ARTEX-local-template-smoke"
''')
        result = run('nuclei', '-u', f'http://127.0.0.1:{server.server_port}',
                     '-t', str(template), '-jsonl', '-silent', '-ni')
        findings = [json.loads(line) for line in result.splitlines() if line.startswith('{')]
        assert any(finding.get('template-id') == 'artex-local-template-smoke' for finding in findings), result
finally:
    server.shutdown()
    server.server_close()
    thread.join()

# Honor an operator's custom template path rather than resetting it on each call.
data['nuclei-templates-directory'] = '/tmp/operator-templates'
config.write_text(json.dumps(data))
run('nuclei', '-version')
assert json.loads(config.read_text())['nuclei-templates-directory'] == '/tmp/operator-templates'
print('Local template execution and preservation of custom template settings: OK', flush=True)
