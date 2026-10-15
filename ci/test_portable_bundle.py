"""Reject incomplete or mismatched cross-build artifacts before image import."""
import copy
import gzip
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("portable_bundle", Path(__file__).resolve().parents[1] / "deploy/portable_bundle.py")
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)


class PortableBundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.lock = {"schema": 1, "go_version": "go1.26.3", "platforms": ["linux/amd64", "linux/ppc64"],
                     "tools": {"fixture": {"version": "1.0.0", "sha256": "1" * 64}}}

    def shard(self, name, targets):
        root = self.root / name
        (root / "archives").mkdir(parents=True)
        (root / "licenses/fixture").mkdir(parents=True)
        (root / "licenses/fixture/LICENSE").write_text("Synthetic test fixture license\n")
        catalog = {"schema": 1, "tools": {"fixture": {"version": "1.0.0", "platforms": {}}}}
        report = {"toolchain": "go1.26.3", "platforms": targets, "results": {}}
        for target in targets:
            path = "archives/fixture_1.0.0_" + target.replace("/", "_") + ".gz"
            binary = ("synthetic binary " + target).encode()
            archive = gzip.compress(binary, mtime=0)
            (root / path).write_bytes(archive)
            catalog["tools"]["fixture"]["platforms"][target] = {"file": path, "format": "gz", "filename": "fixture", "sha256": hashlib.sha256(archive).hexdigest()}
            report["results"]["fixture:" + target] = {"state": "built", "binary_bytes": len(binary), "archive_bytes": len(archive), "binary_sha256": hashlib.sha256(binary).hexdigest()}
        for filename, data in [("catalog.lock.json", catalog), ("build-report.json", report), ("source.lock.json", self.lock)]:
            (root / filename).write_text(json.dumps(data))
        return root

    def test_merge_disjoint_shards_produces_complete_importable_bundle(self):
        shards = [self.shard("a", ["linux/amd64"]), self.shard("b", ["linux/ppc64"])]
        output = self.root / "merged"
        bundle.merge(shards, output, self.lock)
        catalog, report = bundle.validate(output, self.lock)
        self.assertEqual(set(catalog["tools"]["fixture"]["platforms"]), set(self.lock["platforms"]))
        self.assertEqual(len(report["results"]), 2)

    def test_partial_or_duplicate_platforms_do_not_publish_partial_output(self):
        a, b = self.shard("a", ["linux/amd64"]), self.shard("b", ["linux/amd64"])
        for shards in [[a], [a, b]]:
            with self.subTest(shards=shards), self.assertRaises(ValueError):
                bundle.merge(shards, self.root / "merged", self.lock)
            self.assertFalse((self.root / "merged").exists())

    def test_full_import_rejects_valid_but_partial_shard(self):
        a = self.shard("a", ["linux/amd64"])
        with self.assertRaises(ValueError):
            bundle.validate(a, self.lock)

    def test_archive_corruption_and_extra_artifacts_are_rejected(self):
        a = self.shard("a", self.lock["platforms"])
        artifact = next((a / "archives").iterdir())
        original = artifact.read_bytes()
        artifact.write_bytes(original + b"corruption")
        with self.assertRaisesRegex(ValueError, "checksum"):
            bundle.validate(a, self.lock)
        artifact.write_bytes(original)
        (a / "archives/unexpected.gz").write_bytes(original)
        with self.assertRaisesRegex(ValueError, "unexpected"):
            bundle.validate(a, self.lock)

    def test_source_or_build_metadata_mismatch_is_rejected(self):
        a = self.shard("a", self.lock["platforms"])
        changed = copy.deepcopy(self.lock)
        changed["tools"]["fixture"]["sha256"] = "2" * 64
        with self.assertRaisesRegex(ValueError, "source lock"):
            bundle.validate(a, changed)
        path = a / "build-report.json"
        report = json.loads(path.read_text())
        report["results"]["fixture:linux/ppc64"]["state"] = "failed"
        path.write_text(json.dumps(report))
        with self.assertRaisesRegex(ValueError, "build result"):
            bundle.validate(a, self.lock)

    def test_traversal_symlinks_and_oversized_binaries_are_rejected(self):
        a = self.shard("a", self.lock["platforms"])
        path = a / "catalog.lock.json"
        catalog = json.loads(path.read_text())
        catalog["tools"]["fixture"]["platforms"]["linux/amd64"]["file"] = "../../outside"
        path.write_text(json.dumps(catalog))
        with self.assertRaisesRegex(ValueError, "path"):
            bundle.validate(a, self.lock)
        b = self.shard("b", self.lock["platforms"])
        (b / "escape").symlink_to(self.root)
        with self.assertRaisesRegex(ValueError, "symlink"):
            bundle.validate(b, self.lock)
        (b / "escape").unlink()
        from unittest.mock import patch
        with patch.object(bundle, "MAX_BINARY", 1), self.assertRaises(ValueError):
            bundle.validate(b, self.lock)

    def test_license_mismatch_keeps_output_unpublished(self):
        a, b = self.shard("a", ["linux/amd64"]), self.shard("b", ["linux/ppc64"])
        (b / "licenses/fixture/LICENSE").write_text("different source license")
        with self.assertRaisesRegex(ValueError, "licenses differ"):
            bundle.merge([a, b], self.root / "merged", self.lock)
        self.assertFalse((self.root / "merged").exists())
