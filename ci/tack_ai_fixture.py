"""Start only synthetic Tack nodes inside a disposable offline container."""
import json
from pathlib import Path
import signal
import time

from tack_fixture import Fixture

stopping = False


def stop(*args):
    global stopping
    stopping = True


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
with Fixture() as fixture:
    Path('/work/bin').mkdir(exist_ok=True)
    wrapper = Path('/work/bin/tack')
    wrapper.write_text('''#!/usr/local/bin/python3
import json, os, subprocess, sys
p = subprocess.run(['/usr/local/bin/tack', *sys.argv[1:]])
fd = os.open('/work/cli-calls.jsonl', os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
os.write(fd, (json.dumps({'args': sys.argv[1:], 'exit_code': p.returncode})+'\\n').encode())
os.close(fd)
sys.exit(p.returncode)
''')
    wrapper.chmod(0o755)
    Path('/work/context.json').write_text(json.dumps({'server': fixture.admin, 'node_id': 'fixture-a',
        'credential_file': str(fixture.root / 'operator.json'), 'fixture_root': str(fixture.root)}))
    print('synthetic fixture ready', flush=True)
    while not stopping:
        time.sleep(.2)
