from concurrent.futures import ThreadPoolExecutor
import hashlib
import gzip
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from artex_toolkit.catalog import Catalog, clean_stage


class CatalogTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def fixture(self, contents=b"fixture binary", member="tool", duplicate=False, symlink=False):
        archive = self.root / "source.zip"
        with zipfile.ZipFile(archive, "w") as z:
            z.writestr("../../unowned", b"must never escape")
            info = zipfile.ZipInfo(member)
            if symlink:
                info.external_attr = 0o120777 << 16
            z.writestr(info, contents)
            if duplicate:
                z.writestr(member, contents)
        entry = dict(url="https://example.invalid/tool.zip", sha256=hashlib.sha256(archive.read_bytes()).hexdigest(),
                     format="zip", member=member, filename="tool")
        manifest = dict(schema=1, tools={"tool": dict(version="1.0", platforms={"linux/amd64": entry})})
        path = self.root / "catalog.json"
        path.write_text(json.dumps(manifest))
        return Catalog(path, self.root / "cache"), archive

    def test_parallel_publication_and_stage_ownership(self):
        catalog, archive = self.fixture()
        with ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda _: catalog.fetch("tool", "linux/amd64", archive=archive), range(16)))
        self.assertEqual(len({r["path"] for r in results}), 1)
        self.assertFalse((self.root / "unowned").exists())
        receipt = catalog.stage("tool", "linux/amd64", self.root / "stage")
        foreign = self.root / "stage/foreign"
        foreign.write_text("operator data")
        with self.assertRaisesRegex(ValueError, "unowned"):
            clean_stage(self.root / "stage/receipt.json")
        foreign.unlink()
        Path(receipt["path"]).write_text("changed by another process")
        with self.assertRaisesRegex(ValueError, "changed"):
            clean_stage(self.root / "stage/receipt.json")
        Path(receipt["path"]).write_bytes(b"fixture binary")
        clean_stage(self.root / "stage/receipt.json")
        self.assertFalse((self.root / "stage").exists())

    def test_checksum_offline_platform_and_cache_corruption(self):
        catalog, archive = self.fixture()
        with self.assertRaisesRegex(ValueError, "not cached"):
            catalog.fetch("tool", "linux/amd64", offline=True)
        with self.assertRaisesRegex(ValueError, "no pinned"):
            catalog.fetch("tool", "windows/amd64", offline=True)
        wrong = self.root / "wrong"
        wrong.write_bytes(b"corrupt")
        with self.assertRaisesRegex(ValueError, "checksum"):
            catalog.fetch("tool", "linux/amd64", archive=wrong)
        info = catalog.fetch("tool", "linux/amd64", archive=archive)
        Path(info["path"]).write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "integrity"):
            catalog.fetch("tool", "linux/amd64", offline=True)

    def test_archive_symlink_duplicate_and_traversal_rejected(self):
        for kwargs in ({"symlink": True}, {"duplicate": True}):
            catalog, archive = self.fixture(**kwargs)
            with self.assertRaisesRegex(ValueError, "regular"):
                catalog.fetch("tool", "linux/amd64", archive=archive)
        with self.assertRaisesRegex(ValueError, "unsafe"):
            self.fixture(member="../escape")

    def test_tar_exact_member(self):
        catalog, _ = self.fixture()
        manifest = catalog.data
        entry = manifest["tools"]["tool"]["platforms"]["linux/amd64"]
        archive = self.root / "tool.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            info = tarfile.TarInfo("tool")
            info.size = 3
            tar.addfile(info, io.BytesIO(b"yes"))
        entry.update(format="tar.gz", sha256=hashlib.sha256(archive.read_bytes()).hexdigest())
        catalog.path.write_text(json.dumps(manifest))
        catalog = Catalog(catalog.path, catalog.cache)
        result = catalog.fetch("tool", "linux/amd64", archive=archive)
        self.assertEqual(Path(result["path"]).read_bytes(), b"yes")

    def test_gzip_and_raw_limits_leave_no_partial_publication(self):
        for format in ("raw", "gz"):
            with self.subTest(format=format):
                catalog, _ = self.fixture()
                entry = catalog.data["tools"]["tool"]["platforms"]["linux/amd64"]
                archive = self.root / ("bounded." + format)
                archive.write_bytes(gzip.compress(b"x" * 100) if format == "gz" else b"x" * 100)
                entry.update(format=format, sha256=hashlib.sha256(archive.read_bytes()).hexdigest())
                catalog.path.write_text(json.dumps(catalog.data))
                catalog = Catalog(catalog.path, catalog.cache)
                with patch("artex_toolkit.catalog.MAX_BINARY", 10), patch("artex_toolkit.catalog.MAX_ARCHIVE", 50):
                    with self.assertRaisesRegex(ValueError, "size limit"):
                        catalog.fetch("tool", "linux/amd64", archive=archive)
                self.assertFalse((catalog.cache / entry["sha256"]).exists())
                result = catalog.fetch("tool", "linux/amd64", archive=archive)
                self.assertEqual(Path(result["path"]).read_bytes(), b"x" * 100)

    def test_committed_platform_matrix(self):
        catalog = Catalog(Path(__file__).parents[1] / "catalog.lock.json", self.root / "cache")
        for name in ("jq", "ffuf", "httpx", "katana", "interactsh-client"):
            for target in ("linux/amd64", "linux/arm64", "windows/amd64"):
                catalog.select(name, target)
