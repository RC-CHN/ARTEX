from concurrent.futures import ThreadPoolExecutor
import gzip
import hashlib
import io
import ipaddress
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

from artex_toolkit.ipdata import install
from artex_toolkit.iplookup import IPDatabase, describe, inputs, lookup_many


def xdb(network, region):
    """A single-range XDB v3 fixture using the published on-disk format."""
    network = ipaddress.ip_network(network)
    payload = region.encode()
    content = bytearray(256 + 524288)
    position = len(content) + len(payload)
    struct.pack_into('<HHIIIHH', content, 0, 3, 1, 1791936000, position, position, network.version, 4)
    first, last = network.network_address.packed, network.broadcast_address.packed
    struct.pack_into('<II', content, 256 + int.from_bytes(first[:2], 'big') * 8, position, position)
    if network.version == 4:
        first, last = first[::-1], last[::-1]
    return bytes(content) + payload + first + last + struct.pack('<HI', len(payload), len(content))


class IPTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.cache = self.base / 'archives'
        self.cache.mkdir()
        self.root = self.base / 'installed'
        self.path = self.base / 'manifest.json'
        self.manifest = json.loads((Path(__file__).parents[1] / 'ip-data.lock.json').read_text())
        payloads = {'ipv4': xdb('1.0.0.0/24', 'Fixtureland|Province|City|ISP|ZZ'),
                    'ipv6': xdb('2606:4700::/32', 'Fixtureland|Province6|City6|ISP6|ZZ'),
                    'asn': (Path(__file__).parent / 'fixtures/GeoLite2-ASN-Test.mmdb').read_bytes()}
        for name, data in payloads.items():
            raw = gzip.compress(data) if name == 'asn' else data
            entry = self.manifest['sources'][name]
            entry.update(sha256=hashlib.sha256(raw).hexdigest(), size=len(raw),
                         data_sha256=hashlib.sha256(data).hexdigest(), data_size=len(data))
            (self.cache / entry['sha256']).write_bytes(raw)
        self.save()

    def save(self):
        self.path.write_text(json.dumps(self.manifest))

    def install(self):
        return install(self.path, self.root, self.cache, offline=True)

    def test_real_readers_offline_boundaries_mapped_and_missing(self):
        with patch('socket.socket', side_effect=AssertionError('lookup attempted network')):
            self.install()
            with IPDatabase(self.root) as db:
                for ip in ('1.0.0.0', '1.0.0.255', '::ffff:1.0.0.1'):
                    result = db.lookup(ip)
                    self.assertEqual(result['geo']['country'], 'Fixtureland')
                    self.assertEqual(result['asn']['number'], 15169)
                    self.assertEqual(result['asn']['network'], '1.0.0.0/24')
                result = db.lookup('2606:4700:4700::1111')
                self.assertEqual(result['family'], 6)
                self.assertEqual(result['geo']['city'], 'City6')
                self.assertEqual(result['asn']['number'], 237)  # Synthetic fixture, not real attribution.
                self.assertEqual(db.lookup('1.0.1.0')['status'], 'not_found')

    def test_special_and_invalid_literals_do_not_lookup(self):
        with IPDatabase(self.base / 'absent') as db:
            for value, kind in [('10.0.0.1', 'private'), ('fd00::1', 'private'), ('::', 'unspecified'),
                                ('::1', 'loopback'), ('127.0.0.1', 'loopback'), ('fe80::1', 'link_local'),
                                ('224.0.0.1', 'multicast'), ('ff02::1', 'multicast'), ('100.64.1.1', 'shared_address_space'),
                                ('192.0.2.1', 'reserved'), ('2001:db8::1', 'reserved'), ('::ffff:10.1.2.3', 'private')]:
                result = db.lookup(value)
                self.assertEqual(result['address_type'], kind)
                self.assertEqual(result['status'], 'special')
                self.assertIsNone(result['geo'])
            for value in ('example.com', '1.2.3.4:80', '1.0.0.0/24', '1.00.0.1', 'fe80::1%eth0', '999.0.0.1'):
                self.assertEqual(db.lookup(value)['status'], 'invalid')
            self.assertEqual(db.lookup('1.0.0.1')['status'], 'partial')
            self.assertFalse(db.metadata['available'])

    def test_corruption_is_not_a_negative_result(self):
        self.install()
        current = (self.root / 'current').resolve()
        asn = current / 'asn.mmdb'
        asn.write_bytes(b'X' * asn.stat().st_size)
        with IPDatabase(self.root) as db:
            result = db.lookup('1.0.0.1')
            self.assertEqual(result['status'], 'partial')
            self.assertEqual(result['asn']['status'], 'unavailable')
            self.assertEqual(result['geo']['status'], 'found')
        with self.assertRaisesRegex(ValueError, 'integrity'):
            self.install()

    def test_failed_update_keeps_old_snapshot_and_cleans_partial(self):
        first = self.install()['installed']
        self.manifest['release'] = 'candidate'
        self.manifest['sources']['asn']['sha256'] = 'f' * 64
        self.save()
        with self.assertRaisesRegex(ValueError, 'not cached'):
            self.install()
        self.assertEqual((self.root / 'current').resolve().name, first)
        self.assertEqual([p.name for p in (self.root / 'sets').iterdir()], [first])
        self.assertEqual(lookup_many(self.root, ['1.0.0.1'])['results'][0]['asn']['number'], 15169)

    def test_concurrent_install_and_reader_pins_one_generation(self):
        first = self.install()['installed']
        with IPDatabase(self.root) as old:
            self.manifest['release'] = 'next'
            data = xdb('1.0.0.0/24', 'Nextland|Next Province|Next City|Next ISP|YY')
            checksum = hashlib.sha256(data).hexdigest()
            self.manifest['sources']['ipv4'].update(sha256=checksum, data_sha256=checksum,
                                                   size=len(data), data_size=len(data))
            (self.cache / checksum).write_bytes(data)
            self.save()
            with ThreadPoolExecutor(max_workers=4) as pool:
                results = list(pool.map(lambda _: self.install()['installed'], range(4)))
            self.assertEqual(len(set(results)), 1)
            self.assertNotEqual(first, results[0])
            self.assertEqual(old.metadata['snapshot'], first)
            self.assertEqual(old.lookup('1.0.0.1')['asn']['number'], 15169)
            self.assertEqual(old.lookup('1.0.0.1')['geo']['country'], 'Fixtureland')
            self.assertEqual(lookup_many(self.root, ['1.0.0.1'])['results'][0]['geo']['country'], 'Nextland')
            self.assertEqual(describe(self.root)['snapshot'], results[0])
        self.assertTrue((self.root / 'sets' / first).is_dir())

    def test_manifest_traversal_digest_limits_and_gzip_bomb_rejected(self):
        for key, value in [('filename', '../foreign'), ('sha256', 'no-hash'), ('size', 2**30),
                           ('url', 'https://example.invalid/db.mmdb')]:
            with self.subTest(key=key):
                original = self.manifest['sources']['asn'][key]
                self.manifest['sources']['asn'][key] = value
                self.save()
                with self.assertRaises(ValueError):
                    self.install()
                self.manifest['sources']['asn'][key] = original
        self.manifest['sources']['asn']['data_size'] = 10
        self.save()
        with self.assertRaisesRegex(ValueError, 'size limit'):
            self.install()
        self.assertFalse((self.root / 'current').exists())

    def test_input_limits(self):
        self.assertEqual(inputs(['1.0.0.1'], io.StringIO('\n::1\n')), ['1.0.0.1', '::1'])
        for stream in (io.StringIO('1.0.0.1\n' * 10001), io.StringIO('X' * 10000)):
            with self.assertRaises(ValueError):
                inputs([], stream)
        with self.assertRaises(ValueError):
            inputs([])

    def test_old_source_dates_are_visible_and_lookup_does_not_hash(self):
        self.manifest['sources']['ipv6']['date'] = '2000-01-01'
        self.save()
        self.install()
        with patch('artex_toolkit.ipdata.digest', side_effect=AssertionError('per-query full scan')):
            result = lookup_many(self.root, ['2606:4700:4700::1111'])
        self.assertTrue(result['database']['sources']['ipv6']['stale'])
        self.assertEqual(result['database']['sources']['ipv6']['date'], '2000-01-01')


if __name__ == '__main__':
    unittest.main()
