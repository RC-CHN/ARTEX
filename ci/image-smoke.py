#!/usr/bin/env python3
"""Verify an image using disposable PostgreSQL and offline browser tools.

Usage: python3 ci/image-smoke.py IMAGE EXPECTED_VERSION
"""
import http.client
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

image = sys.argv[1]
version = sys.argv[2]
suffix = __import__('uuid').uuid4().hex[:10]
network = 'artex-build-smoke-' + suffix
pg_name = 'artex-build-pg-' + suffix
app_name = 'artex-build-app-' + suffix
created = []
network_created = False

def docker(*args, check=True, **kwargs):
    return subprocess.run(['docker', *args], check=check, text=True,
                          capture_output=True, **kwargs)

def request(path, data=None, token=None):
    headers = {}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    body = None
    if data is not None:
        headers['Content-Type'] = 'application/json'
        body = json.dumps(data).encode()
    with urllib.request.urlopen(urllib.request.Request(base + path, body, headers), timeout=5) as r:
        return r.status, r.read()

try:
    print(docker('run', '--rm', '--network', 'none', '--entrypoint', '/app/artex', image, '-h').stderr.strip(), flush=True)
    docker('network', 'create', network)
    network_created = True
    docker('run', '-d', '--rm', '--name', pg_name, '--network', network,
           '--tmpfs', '/var/lib/postgresql/data', '-e', 'POSTGRES_USER=artex',
           '-e', 'POSTGRES_PASSWORD=artex-smoke-only', '-e', 'POSTGRES_DB=artex', __import__('os').environ.get('ARTEX_SMOKE_PG_IMAGE', 'postgres:16.14-alpine'))
    created.append(pg_name)
    for _ in range(40):
        if docker('exec', pg_name, 'pg_isready', '-h', '127.0.0.1', '-U', 'artex', '-d', 'artex', check=False).returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError('temporary PostgreSQL did not become ready')
    docker('run', '-d', '--rm', '--name', app_name, '--network', network,
           '--tmpfs', '/app/data', '-p', '127.0.0.1::8787', '-e',
           'ARTEX_PG_DSN=postgres://artex:artex-smoke-only@' + pg_name + ':5432/artex?sslmode=disable', image)
    created.append(app_name)
    info = json.loads(docker('inspect', app_name).stdout)[0]
    port = info['NetworkSettings']['Ports']['8787/tcp'][0]['HostPort']
    base = 'http://127.0.0.1:' + port
    for _ in range(80):
        try:
            _, raw = request('/api/health')
            health = json.loads(raw)
            break
        except (urllib.error.URLError, OSError, http.client.HTTPException):
            time.sleep(0.5)
    else:
        raise RuntimeError('ARTEX did not become ready')
    assert health['ok'] and health['version'] == version and health['traffic_backend'] == 'postgres', health
    print('Health and embedded version: OK', flush=True)
    _, page = request('/setup/')
    assert b'<html' in page.lower()
    scripts = re.findall(rb'<script[^>]+src="([^"]+)"', page)
    assert scripts, 'static frontend scripts missing'
    _, asset = request(scripts[0].decode())
    assert len(asset) > 0
    print('Embedded setup page and JavaScript assets: OK', flush=True)
    _, raw = request('/api/auth/status')
    assert json.loads(raw)['initialized'] is False
    try:
        request('/api/tasks')
        raise AssertionError('protected API accepted an anonymous request')
    except urllib.error.HTTPError as e:
        assert e.code == 401, e.code
    _, raw = request('/api/auth/init', {'password': 'ARTEX-smoke-only-2026!'})
    token = json.loads(raw)['token']
    assert token
    _, raw = request('/api/auth/login', {'username': 'ARTEX', 'password': 'ARTEX-smoke-only-2026!'})
    token = json.loads(raw)['token']
    assert token
    for endpoint in ['/api/tasks', '/api/llm/profiles', '/api/stats']:
        status, raw = request(endpoint, token=token)
        assert status == 200
        if endpoint == '/api/stats':
            stats = json.loads(raw)
            assert stats['llm_configured'] is False
            assert stats['traffic_enabled'] is True
    print('PostgreSQL schema, admin setup, authentication and management APIs: OK', flush=True)
    _, raw = request('/api/skills', token=token)
    shipped = {s['name']: s for s in json.loads(raw)['skills']}
    for name in ('nuclei', 'vulnx', 'protocol-check', 'ip-lookup', 'python-environment', 'resource-creator'):
        assert name in shipped and shipped[name]['description'], shipped
        _, raw = request('/api/visibility/skill/' + name, token=token)
        assert len(json.loads(raw)['agents']) >= 4, raw
    print('Bundled native skills and default visibility: OK', flush=True)
    browser = docker('run', '--rm', '--network', network, '--entrypoint', 'node',
                     '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                     '-e', 'BASE_URL=http://' + app_name + ':8787',
                     '-e', 'AUTH_TOKEN=' + token, '-e', 'AUTH_PASSWORD=ARTEX-smoke-only-2026!',
                     image, '/smoke/frontend-smoke.cjs', check=False)
    if browser.returncode:
        print((browser.stdout + browser.stderr).replace(token, '[redacted]'), file=sys.stderr)
        raise RuntimeError('frontend browser smoke failed')
    print(browser.stdout.strip(), flush=True)
    artifact_dir = __import__('os').environ.get('ARTEX_SMOKE_ARTIFACT_DIR')
    artifact_args = []
    if artifact_dir:
        artifact = Path(artifact_dir).resolve()
        artifact.mkdir(parents=True, exist_ok=True)
        artifact_args = ['-v', str(artifact) + ':/artifacts', '-e', 'SCREENSHOT_DIR=/artifacts']
    runners = docker('run', '--rm', '--network', network, '--entrypoint', 'node', *artifact_args,
                     '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                     '-e', 'BASE_URL=http://' + app_name + ':8787', '-e', 'AUTH_TOKEN=' + token,
                     image, '/smoke/runner-ui-smoke.cjs', check=False)
    if runners.returncode:
        print((runners.stdout + runners.stderr).replace(token, '[redacted]'), file=sys.stderr)
        raise RuntimeError('runner UI smoke failed')
    print(runners.stdout.strip(), flush=True)
    resources = docker('run', '--rm', '--network', network, '--entrypoint', 'node',
                       '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                       '-e', 'BASE_URL=http://' + app_name + ':8787', '-e', 'AUTH_TOKEN=' + token,
                       image, '/smoke/resource-ui-smoke.cjs', check=False)
    if resources.returncode:
        print((resources.stdout + resources.stderr).replace(token, '[redacted]'), file=sys.stderr)
        raise RuntimeError('resource UI smoke failed')
    print(resources.stdout.strip(), flush=True)
    _, raw = request('/api/tasks', {'description': 'Synthetic lifecycle UI', 'goal': 'No external execution'}, token)
    lifecycle_task = int(json.loads(raw)['id'])
    # Explicitly synthetic exhausted Worker; no model or external command runs.
    docker('exec', pg_name, 'psql', '-U', 'artex', '-d', 'artex', '-v', 'ON_ERROR_STOP=1', '-c',
           f"WITH n AS (INSERT INTO exploration_nodes(exploration_id,kind,payload,state) SELECT exploration_id,'intent','{{\"summary\":\"synthetic budget fixture\"}}','exhausted' FROM tasks WHERE id={lifecycle_task} RETURNING id) INSERT INTO worker_budgets(intent_id,task_id,normal_limit,wrapup_limit,normal_used,wrapup_used,phase) SELECT id,{lifecycle_task},150,10,150,10,'exhausted' FROM n")
    lifecycle = docker('run', '--rm', '--network', network, '--entrypoint', 'node', *artifact_args,
                       '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                       '-e', 'BASE_URL=http://' + app_name + ':8787', '-e', 'AUTH_TOKEN=' + token,
                       '-e', 'LIFECYCLE_TASK_ID=' + str(lifecycle_task), '-e', 'EXPECT_HTTP_ORIGIN=1', image, '/smoke/lifecycle-ui-smoke.cjs', check=False)
    if lifecycle.returncode:
        print((lifecycle.stdout + lifecycle.stderr).replace(token, '[redacted]'), file=sys.stderr)
        raise RuntimeError('lifecycle UI smoke failed')
    print(lifecycle.stdout.strip(), flush=True)
    # Record a local request through the capture proxy, then read and search it.
    docker('exec', app_name, 'curl', '-fsS', '--noproxy', '', '--proxy', 'http://127.0.0.1:8788', 'http://127.0.0.1:8787/api/health?smoke=postgres-capture')
    for _ in range(40):
        _, raw = request('/api/traffic?body=postgres-capture', token=token)
        captures = json.loads(raw)
        if captures.get('total', 0) > 0:
            break
        time.sleep(0.1)
    assert captures['total'] == 1, captures
    capture_id = captures['exchanges'][0]['id']
    _, raw = request('/api/traffic/exchange?id=' + capture_id, token=token)
    assert 'postgres' in json.loads(raw)['resp']
    count = docker('exec', pg_name, 'psql', '-U', 'artex', '-d', 'artex', '-Atc', 'SELECT count(*) FROM artex_traffic.exchanges').stdout.strip()
    assert count == '1', count
    docker('exec', app_name, 'test', '!', '-e', '/app/data/traffic/_index/index.sqlite')
    print('PostgreSQL capture, body search, evidence read and no SQLite writes: OK', flush=True)
    for script in ['verify.cjs', 'verify-mcp.cjs']:
        r = docker('run', '--rm', '--network', 'none', '--entrypoint', 'node', image, '/opt/artex-tools/' + script)
        print(r.stdout.strip(), flush=True)
    docker('run', '--rm', '--network', 'none', '--entrypoint', 'playwright-cli', image, '--help')
    print('Bundled Playwright CLI: OK', flush=True)
    security = docker('run', '--rm', '--network', 'none', '--read-only', '--user', '10001:10001',
                      '--tmpfs', '/tmp:rw,mode=1777', '-e', 'HOME=/tmp/artex-security-smoke',
                      '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                      '--entrypoint', 'python3', image, '/smoke/security-tools-smoke.py', check=False)
    if security.returncode:
        print(security.stdout + security.stderr, file=sys.stderr)
        raise RuntimeError('security tools smoke failed')
    print(security.stdout.strip(), flush=True)
    python_env = docker('run', '--rm', '--network', 'none', '--read-only', '--user', '10001:10001',
                        '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--memory', '2g',
                        '--tmpfs', '/tmp:rw,mode=1777', '-e', 'HOME=/tmp/artex-python-smoke',
                        '-e', 'PYTHONPATH=/opt/artex-kit',
                        '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                        '--entrypoint', 'python3', image, '/smoke/python-environment-smoke.py', check=False)
    if python_env.returncode:
        print(python_env.stdout + python_env.stderr, file=sys.stderr)
        raise RuntimeError('Python environment smoke failed')
    print(python_env.stdout.strip(), flush=True)
    toolkit = docker('run', '--rm', '--network', 'none', '--read-only', '--user', '10001:10001',
                     '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                     '--tmpfs', '/tmp:rw,mode=1777', '-e', 'HOME=/tmp/artex-toolkit-smoke',
                     '-v', str(Path(__file__).resolve().parent) + ':/smoke:ro',
                     '-v', str(Path(__file__).resolve().parents[1] / 'skills/api-recon/scripts') + ':/api-recon:ro',
                     '--entrypoint', 'python3', image, '/smoke/toolkit-image-smoke.py', check=False)
    if toolkit.returncode:
        print(toolkit.stdout + toolkit.stderr, file=sys.stderr)
        raise RuntimeError('toolkit image smoke failed')
    print(toolkit.stdout.strip(), flush=True)
    print('IMAGE SMOKE PASSED', flush=True)
except Exception:
    if app_name in created:
        logs = docker('logs', '--tail', '40', app_name, check=False)
        print(logs.stdout + logs.stderr, file=sys.stderr)
    raise
finally:
    for name in reversed(created):
        docker('rm', '-f', '-v', name, check=False)
    if network_created:
        docker('network', 'rm', network, check=False)
