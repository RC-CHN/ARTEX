"""Private loopback Tack processes for synthetic CLI/model tests only."""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


class Fixture:
    def __init__(self, binaries=None):
        self.binaries = Path(binaries) if binaries else None
        self.temp = tempfile.TemporaryDirectory(prefix='artex-tack-fixture-')
        self.root = Path(self.temp.name)
        self.processes = []
        self.logs = []
        self.admin = 'http://127.0.0.1:' + str(free_port())
        self.node_url = 'http://127.0.0.1:' + str(free_port())
        self.admin_token = secrets.token_hex(32)
        self.node_token = secrets.token_hex(32)
        self.operator_token = secrets.token_hex(32)

    def binary(self, name):
        return str(self.binaries / name) if self.binaries else name

    def write(self, name, value):
        path = self.root / name
        path.write_text(json.dumps(value))
        path.chmod(0o600)
        return path

    def spawn(self, name, *args):
        log = (self.root / (name + '.log')).open('wb')
        self.logs.append(log)
        proc = subprocess.Popen([self.binary(name), *map(str, args)], stdout=log, stderr=log, cwd=self.root)
        self.processes.append(proc)
        return proc

    def api(self, method, path, value=None, token=None):
        req = urllib.request.Request(self.admin + path, method=method,
                                     data=None if value is None else json.dumps(value).encode(),
                                     headers={'Authorization': 'Bearer ' + (token or self.admin_token), 'Content-Type': 'application/json'})
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=5) as response:
            return json.load(response)

    def cli(self, command, *args, check=True):
        proc = subprocess.run([self.binary('tack'), command, '--server', self.admin,
                               '--credential', str(self.root / 'operator.json'), '--allow-insecure-loopback', *map(str, args)],
                              cwd=self.root, text=True, capture_output=True, timeout=20)
        if check and proc.returncode:
            raise AssertionError((command, proc.returncode, proc.stdout, proc.stderr))
        return proc

    def __enter__(self):
        try:
            config = self.write('controller.json', {'admin_token': self.admin_token, 'nodes': [
                {'id': 'fixture-a', 'token': self.node_token}, {'id': 'fixture-offline', 'token': secrets.token_hex(32)}]})
            self.write('operator.json', {'token': self.operator_token})
            for name in ('state', 'work'):
                (self.root / name).mkdir()
            node = self.write('node.json', {'server': self.node_url, 'node_id': 'fixture-a', 'token': self.node_token,
                'state_dir': str(self.root / 'state'), 'work_dir': str(self.root / 'work'),
                'heartbeat_seconds': 2, 'allow_insecure_loopback': True})
            self.spawn('tack-control', '-config', config, '-state', self.root / 'tasks.json',
                       '-listen', self.node_url.removeprefix('http://'), '-admin-listen', self.admin.removeprefix('http://'))
            deadline = time.monotonic() + 10
            while True:
                try:
                    self.api('GET', '/v1/nodes')
                    break
                except OSError:
                    if time.monotonic() > deadline:
                        raise
                    time.sleep(.05)
            self.api('POST', '/v1/operators', {'id': 'fixture-operator', 'token': self.operator_token,
                'node_ids': ['fixture-a'], 'read': True, 'execute': True})
            self.spawn('tack-node', '-config', node)
            while time.monotonic() < deadline:
                nodes = self.api('GET', '/v1/nodes')
                if any(n['id'] == 'fixture-a' and n.get('status', {}).get('accepting_tasks') for n in nodes):
                    return self
                time.sleep(.05)
            raise RuntimeError('synthetic node did not become ready')
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def __exit__(self, *unused):
        for proc in reversed(self.processes):
            if proc.poll() is None:
                proc.terminate()
            try:
                proc.wait(timeout=12)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        for log in self.logs:
            log.close()
        self.temp.cleanup()
