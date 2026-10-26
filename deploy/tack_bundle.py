#!/usr/bin/env python3
"""Bundle pinned Tack releases without executing or unpacking foreign binaries."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
import urllib.request

MAX_FILE = 256 * 1024 * 1024


def digest(path):
    with Path(path).open('rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def install(lock_path, output, archives=None, offline=False):
    lock = json.loads(Path(lock_path).read_text())
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    files = dict(lock['metadata'])
    catalog = {'schema': 1, 'tools': {}}
    for name, tool in lock['tools'].items():
        local = {**tool, 'platforms': {}}
        for platform, entry in tool['platforms'].items():
            filename = entry['url'].rsplit('/', 1)[-1]
            files[filename] = {'url': entry['url'], 'sha256': entry['sha256']}
            local['platforms'][platform] = {k: v for k, v in entry.items() if k != 'url'} | {'file': filename}
        catalog['tools'][name] = local
    for filename, entry in files.items():
        if Path(filename).name != filename or filename in ('', '.', '..') or '\\' in filename:
            raise ValueError('unsafe bundle filename')
        if not entry['url'].startswith('https://github.com/RC-CHN/tack/releases/download/' + lock['release'] + '/'):
            raise ValueError('unpinned Tack release URL')
        if len(entry['sha256']) != 64 or any(c not in '0123456789abcdef' for c in entry['sha256']):
            raise ValueError('invalid checksum')

    def fetch(item):
        filename, entry = item
        dest = output / filename
        if dest.is_file() and not dest.is_symlink() and digest(dest) == entry['sha256']:
            return
        cached = None
        if archives:
            cached = next((p for p in (Path(archives) / entry['sha256'], Path(archives) / filename) if p.is_file()), None)
        if not cached and offline:
            raise ValueError('missing offline artifact: ' + filename)
        fd, temp = tempfile.mkstemp(prefix='.download-', dir=output)
        os.close(fd)
        try:
            for attempt in range(4):
                try:
                    with (cached.open('rb') if cached else urllib.request.urlopen(entry['url'], timeout=60)) as src, open(temp, 'wb') as dst:
                        if not cached and not src.url.startswith('https://'):
                            raise ValueError('insecure download redirect')
                        size = 0
                        while chunk := src.read(1024 * 1024):
                            size += len(chunk)
                            if size > MAX_FILE:
                                raise ValueError('artifact exceeds size limit')
                            dst.write(chunk)
                    break
                except (OSError, TimeoutError):
                    if cached or attempt == 3:
                        raise
                    time.sleep(attempt + 1)
            if digest(temp) != entry['sha256']:
                raise ValueError('checksum mismatch: ' + filename)
            os.chmod(temp, 0o644)
            os.replace(temp, dest)
        finally:
            Path(temp).unlink(missing_ok=True)

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(fetch, files.items()))
    # Publish the catalog only once every archive and metadata file is verified.
    with tempfile.NamedTemporaryFile(mode='w', prefix='.catalog-', dir=output, delete=False) as f:
        json.dump(catalog, f, indent=2)
        f.write('\n')
    os.chmod(f.name, 0o644)
    os.replace(f.name, output / 'catalog.lock.json')
    return {'release': lock['release'], 'archives': sum(len(t['platforms']) for t in catalog['tools'].values()),
            'files': len(files), 'bytes': sum((output / name).stat().st_size for name in files)}


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--lock', default='toolkit/tack.lock.json')
    p.add_argument('--output', required=True)
    p.add_argument('--archives', default=os.environ.get('ARTEX_TOOL_ARCHIVES'))
    p.add_argument('--offline', action='store_true')
    a = p.parse_args()
    print(json.dumps(install(a.lock, a.output, a.archives, a.offline)))
