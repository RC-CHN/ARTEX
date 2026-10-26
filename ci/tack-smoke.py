#!/usr/bin/env python3
"""Exercise exact bundled Tack release binaries on isolated loopback nodes."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import urllib.error

from tack_fixture import Fixture


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--bin-dir')
    p.add_argument('--bundle', default='/opt/artex-kit/tack')
    a = p.parse_args()
    bundle = Path(a.bundle)
    catalog = json.loads((bundle / 'catalog.lock.json').read_text())
    for tool in catalog['tools'].values():
        for entry in tool['platforms'].values():
            with (bundle / entry['file']).open('rb') as f:
                assert hashlib.file_digest(f, 'sha256').hexdigest() == entry['sha256']
    assert sum(len(t['platforms']) for t in catalog['tools'].values()) == 33
    sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'toolkit'))
    if Path('/opt/artex-kit').exists():
        sys.path.insert(0, '/opt/artex-kit')
    from artex_toolkit.catalog import Catalog
    with tempfile.TemporaryDirectory() as cache:
        c = Catalog(bundle / 'catalog.lock.json', cache)
        for name, platform in [('tack-node', 'linux/mipsle'), ('tack-node-legacy', 'linux/amd64'), ('tack', 'windows/arm64')]:
            result = c.fetch(name, platform, offline=True)
            assert Path(result['path']).stat().st_size > 0
    with Fixture(a.bin_dir) as f:
        for name in ('tack', 'tack-node', 'tack-control'):
            assert '0.1.2' in subprocess.check_output([f.binary(name), '-version'], text=True)
        nodes = json.loads(f.cli('nodes').stdout)
        assert len(nodes) == 1 and nodes[0]['id'] == 'fixture-a'
        request = f.write('once.json', {'node_id': 'fixture-a', 'caller_id': 'smoke-once', 'command': {
            'mode': 'posix-sh', 'script': 'printf x >> once.marker; printf "ready\\n"', 'dir': str(f.root),
            'timeout_seconds': 5, 'output_limit': 1000}})
        first = json.loads(f.cli('submit', '--file', request).stdout)
        saved = json.loads(request.read_text()); assert saved['request_id']
        with ThreadPoolExecutor(max_workers=6) as pool:
            duplicates = list(pool.map(lambda _: f.api('POST', '/v1/tasks', saved, f.operator_token), range(12)))
        assert all(t['id'] == first['id'] for t in duplicates)
        result = json.loads(f.cli('wait', '--id', first['id'], '--timeout', '5s', '--check', '--format', 'json').stdout)
        assert result['state'] == 'succeeded' and not result.get('result_pending', False)
        assert result['result']['stdout'] == 'ready\n' and (f.root / 'once.marker').read_text() == 'x'
        saved['command']['script'] = 'exit 7'
        try:
            f.api('POST', '/v1/tasks', saved, f.operator_token)
            raise AssertionError('conflicting identity accepted')
        except urllib.error.HTTPError as e:
            assert e.code == 409 and json.load(e)['error_code'] == 'request_conflict'
        bad = f.write('bad.json', {'node_id': 'fixture-a', 'command': {'mode': 'posix-sh', 'script': 'exit 7'}})
        task = json.loads(f.cli('submit', '--file', bad).stdout)
        failure = f.cli('wait', '--id', task['id'], '--timeout', '5s', '--check', '--format', 'json', check=False)
        assert failure.returncode == 3 and json.loads(failure.stdout)['result']['exit_code'] == 7
        slow = f.write('slow.json', {'node_id': 'fixture-a', 'command': {'mode': 'posix-sh', 'script': 'sleep 20', 'timeout_seconds': 30}})
        task = json.loads(f.cli('submit', '--file', slow).stdout)
        waiting = f.cli('wait', '--id', task['id'], '--timeout', '250ms', '--check', '--format', 'json', check=False)
        assert waiting.returncode == 4
        current = json.loads(f.cli('get', '--id', task['id'], '--format', 'json').stdout)
        assert not current['cancel_requested'] and current['state'] in ('queued', 'dispatched', 'running')
        f.cli('cancel', '--id', task['id'], '--format', 'json')
        canceled = f.cli('wait', '--id', task['id'], '--timeout', '5s', '--check', '--format', 'json', check=False)
        assert canceled.returncode == 3 and json.loads(canceled.stdout)['state'] == 'canceled'
        try:
            f.api('POST', '/v1/tasks', {'node_id': 'fixture-offline', 'command': {'mode': 'posix-sh', 'script': 'true'}}, f.operator_token)
            raise AssertionError('operator crossed node scope')
        except urllib.error.HTTPError as e:
            assert e.code == 403
    print('Tack: 33 archives, offline foreign staging, real CLI/node execution, concurrent idempotency, conflict, exit status, wait timeout, cancellation and operator scope: OK')


if __name__ == '__main__':
    main()
