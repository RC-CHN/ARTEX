"""Start only synthetic Tack nodes inside a disposable offline container."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import signal
import threading
import time
import urllib.error
import urllib.request

from tack_fixture import Fixture

stopping = False


def stop(*args):
    global stopping
    stopping = True


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
with Fixture() as fixture:
    lock = threading.Lock()
    class Audit(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            self.forward()

        def do_POST(self):
            self.forward()

        def forward(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
            request = urllib.request.Request(fixture.admin + self.path, method=self.command,
                data=body if self.command == 'POST' else None,
                headers={'Authorization': self.headers.get('Authorization', ''), 'Content-Type': 'application/json'})
            try:
                response = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=10)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                data = response.read()
                status = response.status
            value = json.loads(data)
            entry = {'method': self.command, 'path': self.path, 'http_status': status}
            if isinstance(value, dict):
                entry.update({k: value[k] for k in ('id', 'request_id', 'state', 'cancel_requested', 'result_pending') if k in value})
            with lock, Path('/work/api-calls.jsonl').open('a') as log:
                log.write(json.dumps(entry) + '\n')
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
    audit = ThreadingHTTPServer(('127.0.0.1', 0), Audit)
    thread = threading.Thread(target=audit.serve_forever, daemon=True)
    thread.start()
    Path('/work/context.json').write_text(json.dumps({'server': 'http://127.0.0.1:' + str(audit.server_port), 'node_id': 'fixture-a',
        'credential_file': str(fixture.root / 'operator.json'), 'fixture_root': str(fixture.root)}))
    print('synthetic fixture ready', flush=True)
    while not stopping:
        time.sleep(.2)
    audit.shutdown()
    audit.server_close()
    thread.join()
