"""Explicit, checksum-pinned IP data installation; lookup never calls this module."""
from contextlib import ExitStack
from datetime import date
import fcntl
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import tempfile
import urllib.parse
import urllib.request
import uuid

from .catalog import atomic_json, copy_bounded, digest
from .iplookup import FILENAMES, open_reader

MAX_FILE = 128 * 1024 * 1024
MAX_TOTAL = 256 * 1024 * 1024
ORIGINS = {
    "ipv4": "https://raw.githubusercontent.com/lionsoul2014/ip2region/",
    "ipv6": "https://raw.githubusercontent.com/lionsoul2014/ip2region/",
    "asn": "https://download.db-ip.com/free/",
}


def load_manifest(path):
    path = Path(path)
    if path.stat().st_size > 65536:
        raise ValueError("IP data manifest exceeds size limit")
    manifest = json.loads(path.read_text())
    if manifest.get("schema") != 1 or set(manifest.get("sources", {})) != set(FILENAMES):
        raise ValueError("IP data manifest requires ipv4, ipv6 and asn sources")
    total = 0
    for name, entry in manifest["sources"].items():
        if entry["filename"] != FILENAMES[name] or entry["format"] != ("gz" if name == "asn" else "raw"):
            raise ValueError("unexpected IP data filename or format")
        url = urllib.parse.urlsplit(entry["url"])
        if not entry["url"].startswith(ORIGINS[name]) or url.username or url.password or url.query or url.fragment:
            raise ValueError("IP data must use a public upstream HTTPS URL without credentials")
        for key in ("sha256", "data_sha256"):
            if not re.fullmatch(r"[a-f0-9]{64}", entry[key]):
                raise ValueError("IP data requires SHA-256 checksums")
        for key in ("size", "data_size"):
            if type(entry[key]) is not int or not 0 < entry[key] <= MAX_FILE:
                raise ValueError("IP data exceeds size limit")
        date.fromisoformat(entry["date"])
        for key in ("provider", "license", "attribution_url"):
            if not isinstance(entry[key], str) or not entry[key] or len(entry[key]) > 512:
                raise ValueError("IP data source attribution is required")
        total += entry["data_size"]
    if total > MAX_TOTAL:
        raise ValueError("IP database set exceeds size limit")
    return manifest


def validate_set(path, manifest):
    """Full verification is reserved for installs, never repeated on lookup."""
    with ExitStack() as stack:
        for name, entry in manifest["sources"].items():
            file = path / entry["filename"]
            if file.is_symlink() or file.stat().st_size != entry["data_size"] or digest(file) != entry["data_sha256"]:
                raise ValueError("IP database failed integrity verification: " + name)
            reader = open_reader(name, file)
            stack.callback(reader.close)
            # Exercise native readers for both IP families before publication.
            if name == "asn":
                reader.get("8.8.8.8")
                reader.get("2001:4860:4860::8888")
            else:
                reader.search("8.8.8.8" if name == "ipv4" else "2001:4860:4860::8888")


def install(manifest_path, root, archives=None, offline=False):
    manifest = load_manifest(manifest_path)
    key = hashlib.sha256(json.dumps(manifest, sort_keys=True).encode()).hexdigest()
    root = Path(root).resolve()
    root.mkdir(parents=True, exist_ok=True)
    sets = root / "sets"
    if sets.is_symlink():
        raise ValueError("refusing symlink snapshot directory")
    sets.mkdir(exist_ok=True)
    final = sets / key
    # Only updates lock; readers hold an already-resolved immutable version.
    lock_fd = os.open(root / ".update.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if final.exists():
            if final.is_symlink():
                raise ValueError("refusing symlink snapshot")
            validate_set(final, manifest)
            if json.loads((final / "manifest.json").read_text()) != manifest:
                raise ValueError("existing snapshot metadata mismatch")
        else:
            with tempfile.TemporaryDirectory(prefix=".install-", dir=sets) as temp:
                stage = Path(temp)
                for name, entry in manifest["sources"].items():
                    archive = stage / (name + ".download")
                    cached = Path(archives) / entry["sha256"] if archives else None
                    with archive.open("wb") as out:
                        if cached and cached.is_file():
                            with cached.open("rb") as source:
                                copy_bounded(source, out, entry["size"])
                        elif offline:
                            raise ValueError("IP data is not cached: " + name)
                        else:
                            request = urllib.request.Request(entry["url"], headers={"User-Agent": "ARTEX-IP-Data-Updater/1.0"})
                            with urllib.request.urlopen(request, timeout=60) as source:
                                if urllib.parse.urlsplit(source.url).scheme != "https":
                                    raise ValueError("IP data redirected away from HTTPS")
                                copy_bounded(source, out, entry["size"])
                    if archive.stat().st_size != entry["size"] or digest(archive) != entry["sha256"]:
                        raise ValueError("IP archive checksum mismatch: " + name)
                    target = stage / entry["filename"]
                    with (gzip.open(archive, "rb") if entry["format"] == "gz" else archive.open("rb")) as source, target.open("wb") as out:
                        copy_bounded(source, out, entry["data_size"])
                        out.flush()
                        os.fsync(out.fileno())
                    target.chmod(0o644)
                    archive.unlink()
                validate_set(stage, manifest)
                licenses = Path(__file__).resolve().parent.parent / "licenses"
                for provider in ("ip2region", "db-ip"):
                    shutil.copyfile(licenses / provider / "LICENSE", stage / (provider + "-LICENSE"))
                atomic_json(stage / "manifest.json", manifest)
                (stage / "manifest.json").chmod(0o644)
                stage.chmod(0o755)
                fd = os.open(stage, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(fd)
                finally:
                    os.close(fd)
                stage.rename(final)
                fd = os.open(sets, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(fd)
                finally:
                    os.close(fd)
        pointer = root / (".current-" + uuid.uuid4().hex)
        try:
            pointer.symlink_to("sets/" + key)
            os.replace(pointer, root / "current")
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        finally:
            pointer.unlink(missing_ok=True)
    return {"installed": key, "directory": str(root), "bytes": sum(e["data_size"] for e in manifest["sources"].values())}
