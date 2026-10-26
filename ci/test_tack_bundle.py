import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('tack_bundle', ROOT / 'deploy/tack_bundle.py')
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)
select_spec = importlib.util.spec_from_file_location('tack_select', ROOT / 'skills/tack/scripts/select-node-artifact.py')
selector = importlib.util.module_from_spec(select_spec)
select_spec.loader.exec_module(selector)


class TackBundleTest(unittest.TestCase):
    def test_locked_matrix_and_skill_links(self):
        lock = json.loads((ROOT / 'toolkit/tack.lock.json').read_text())
        self.assertEqual({k: len(v['platforms']) for k, v in lock['tools'].items()},
                         {'tack': 7, 'tack-control': 3, 'tack-node': 13, 'tack-node-legacy': 10})
        self.assertFalse(lock['tools']['tack-node-legacy']['native_install'])
        import re
        for path in (ROOT / 'skills/tack').rglob('*.md'):
            for target in re.findall(r'\]\(([^)]+)\)', path.read_text()):
                if not target.startswith('https://'):
                    self.assertTrue((path.parent / target).is_file(), (path, target))

    def test_offline_integrity_and_no_catalog_on_partial_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); cache = root / 'cache'; cache.mkdir()
            artifact = cache / 'tack.tar.gz'; artifact.write_bytes(b'synthetic archive')
            digest = hashlib.sha256(artifact.read_bytes()).hexdigest()
            lock = {'release': 'tack-v0.1.2', 'metadata': {}, 'tools': {'tack': {'version': '0.1.2', 'platforms': {
                'linux/amd64': {'url': 'https://github.com/RC-CHN/tack/releases/download/tack-v0.1.2/tack.tar.gz',
                                'sha256': digest, 'format': 'tar.gz', 'member': 'tack', 'filename': 'tack'}}}}}
            source = root / 'lock.json'; source.write_text(json.dumps(lock))
            out = root / 'bundle'
            with self.assertRaisesRegex(ValueError, 'missing offline'):
                bundle.install(source, out, offline=True)
            self.assertFalse((out / 'catalog.lock.json').exists())
            artifact.write_bytes(b'corrupt')
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                bundle.install(source, out, cache, offline=True)
            self.assertFalse((out / 'catalog.lock.json').exists())
            artifact.write_bytes(b'synthetic archive')
            result = bundle.install(source, out, cache, offline=True)
            self.assertEqual(result['archives'], 1)
            self.assertEqual(bundle.install(source, out, offline=True), result)
            local = json.loads((out / 'catalog.lock.json').read_text())
            self.assertEqual(local['tools']['tack']['platforms']['linux/amd64']['file'], 'tack.tar.gz')
            self.assertFalse(any(p.name.startswith('.') for p in out.iterdir()))

    def test_selector_refuses_unsupported_devices(self):
        for args in [('linux', 'mips', '2.6.32', None), ('windows', 'amd64', None, '6.1'), ('linux', 'armv5', '2.6.32', None)]:
            with self.subTest(args=args), self.assertRaises(ValueError):
                selector.select({'format': 'tack-release-v2', 'artifacts': []}, *args)
