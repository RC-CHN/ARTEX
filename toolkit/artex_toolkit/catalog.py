"""Checksum-addressed, bounded artifact installation. No archive extractall()."""
import hashlib
import gzip
import json
import os
from pathlib import Path
import platform
import re
import shutil
import tarfile
import tempfile
import urllib.parse
import urllib.request
import zipfile

MAX_ARCHIVE = 256 * 1024 * 1024
MAX_BINARY = 256 * 1024 * 1024
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,100}\Z")


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def host_platform():
    arch = {"x86_64": "amd64", "aarch64": "arm64", "AMD64": "amd64"}.get(platform.machine(), platform.machine())
    return platform.system().lower() + "/" + arch


def atomic_json(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".write-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    finally:
        Path(tmp).unlink(missing_ok=True)


def copy_bounded(source, dest, limit):
    size = 0
    while chunk := source.read(min(1024 * 1024, limit - size + 1)):
        size += len(chunk)
        if size > limit:
            raise ValueError("artifact exceeds size limit")
        dest.write(chunk)
    return size


class Catalog:
    def __init__(self, path, cache):
        self.path = Path(path)
        self.data = json.loads(self.path.read_text())
        if self.data.get("schema") != 1:
            raise ValueError("unsupported catalog schema")
        self.cache = Path(cache)
        for name, tool in self.data["tools"].items():
            if not NAME.fullmatch(name) or not NAME.fullmatch(tool["version"]):
                raise ValueError("invalid tool name/version")
            for target, entry in tool["platforms"].items():
                if not re.fullmatch(r"(linux|windows|darwin)/(amd64|arm64|386|armv[567]|mips|mipsle|mips64|mips64le|ppc64|ppc64le|riscv64|s390x|loong64)", target):
                    raise ValueError("invalid platform")
                if not re.fullmatch(r"[a-f0-9]{64}", entry["sha256"]):
                    raise ValueError("invalid digest")
                if not NAME.fullmatch(entry["filename"]):
                    raise ValueError("invalid filename")
                if "url" in entry:
                    url = urllib.parse.urlsplit(entry["url"])
                    if url.scheme != "https" or not url.hostname or url.username or url.password:
                        raise ValueError("artifacts require credential-free HTTPS URLs")
                elif "file" in entry:
                    local = Path(entry["file"])
                    if local.is_absolute() or ".." in local.parts or "\\" in entry["file"]:
                        raise ValueError("local artifact must stay within catalog directory")
                else:
                    raise ValueError("artifact needs a URL or relative file")
                if entry["format"] not in ("zip", "tar.gz", "gz", "raw"):
                    raise ValueError("unsupported artifact format")
                member = entry.get("member", "")
                if member.startswith("/") or ".." in member.split("/") or "\\" in member:
                    raise ValueError("unsafe archive member")

    def select(self, name, target):
        try:
            return self.data["tools"][name], self.data["tools"][name]["platforms"][target]
        except KeyError:
            raise ValueError(f"no pinned artifact for {name} on {target}") from None

    def fetch(self, name, target, offline=False, archive=None):
        tool, entry = self.select(name, target)
        # A private writable cache; an operator may instead supply a prepared
        # read-only cache. Cache hits are verified again before use.
        directory = self.cache / entry["sha256"]
        binary = directory / entry["filename"]
        receipt = directory / "receipt.json"
        bundled = Path("/opt/artex-kit/cache")
        if not binary.exists() and self.cache != bundled and (bundled / entry["sha256"]).is_dir():
            return Catalog(self.path, bundled).fetch(name, target, offline=True)
        if binary.is_file() and receipt.is_file():
            info = json.loads(receipt.read_text())
            if (not binary.is_symlink() and not directory.is_symlink()
                    and info.get("archive_sha256") == entry["sha256"]
                    and info.get("binary_sha256") == digest(binary)):
                return {**info, "path": str(binary.resolve())}
            raise ValueError("cached artifact failed integrity verification")
        if archive is None and "file" in entry:
            candidate = (self.path.parent / entry["file"]).resolve()
            if not candidate.is_relative_to(self.path.parent.resolve()):
                raise ValueError("local artifact escaped catalog directory")
            archive = candidate
        if archive is None and os.environ.get("ARTEX_TOOL_ARCHIVES"):
            candidate = Path(os.environ["ARTEX_TOOL_ARCHIVES"]) / entry["sha256"]
            if candidate.is_file():
                archive = candidate
        if offline and archive is None:
            raise ValueError("artifact is not cached; explicitly fetch it before offline execution")
        self.cache.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix=".fetch-", dir=self.cache) as temp:
            temp = Path(temp)
            downloaded = temp / "archive"
            with downloaded.open("wb") as out:
                if archive:
                    with Path(archive).open("rb") as source:
                        copy_bounded(source, out, MAX_ARCHIVE)
                else:
                    with urllib.request.urlopen(entry["url"], timeout=60) as source:
                        if urllib.parse.urlsplit(source.url).scheme != "https":
                            raise ValueError("artifact redirected away from HTTPS")
                        copy_bounded(source, out, MAX_ARCHIVE)
            if digest(downloaded) != entry["sha256"]:
                raise ValueError("artifact checksum mismatch")
            publish = temp / "publish"
            publish.mkdir()
            dest = publish / entry["filename"]
            if entry["format"] == "raw":
                shutil.copyfile(downloaded, dest)
            elif entry["format"] == "gz":
                with gzip.open(downloaded, "rb") as source, dest.open("wb") as out:
                    copy_bounded(source, out, MAX_BINARY)
            elif entry["format"] == "zip":
                with zipfile.ZipFile(downloaded) as z:
                    matches = [i for i in z.infolist() if i.filename == entry["member"]]
                    if len(matches) != 1 or matches[0].is_dir() or (matches[0].external_attr >> 16) & 0o170000 == 0o120000:
                        raise ValueError("expected one regular binary member")
                    with z.open(matches[0]) as source, dest.open("wb") as out:
                        copy_bounded(source, out, MAX_BINARY)
            else:
                with tarfile.open(downloaded, "r:gz") as tar:
                    matches = [i for i in tar if i.name == entry["member"]]
                    if len(matches) != 1 or not matches[0].isfile():
                        raise ValueError("expected one regular binary member")
                    with tar.extractfile(matches[0]) as source, dest.open("wb") as out:
                        copy_bounded(source, out, MAX_BINARY)
            dest.chmod(0o755)
            # Ship upstream notices beside the tool, without extracting paths
            # supplied by an archive. Raw release assets use committed notices.
            notices = {}
            if entry["format"] == "zip":
                with zipfile.ZipFile(downloaded) as z:
                    for item in z.infolist():
                        base = Path(item.filename).name
                        if NAME.fullmatch(base) and base.lower().startswith(("license", "notice", "copying")) and not item.is_dir() and item.file_size <= 65536:
                            notices[base] = z.read(item)
            elif entry["format"] == "tar.gz":
                with tarfile.open(downloaded) as tar:
                    for item in tar:
                        base = Path(item.name).name
                        if NAME.fullmatch(base) and base.lower().startswith(("license", "notice", "copying")) and item.isfile() and item.size <= 65536:
                            notices[base] = tar.extractfile(item).read()
            committed = self.path.parent / "licenses" / name
            if committed.is_dir():
                for item in committed.iterdir():
                    if NAME.fullmatch(item.name) and item.is_file() and item.stat().st_size <= 65536:
                        notices[item.name] = item.read_bytes()
            for filename, content in list(notices.items())[:8]:
                if filename != entry["filename"]:
                    (publish / filename).write_bytes(content)
            files = {p.name: digest(p) for p in publish.iterdir() if p.is_file()}
            info = dict(schema=1, tool=name, version=tool["version"], platform=target,
                        filename=entry["filename"], archive_sha256=entry["sha256"],
                        binary_sha256=digest(dest), size=dest.stat().st_size,
                        abi=entry.get("abi", "see upstream compatibility"), files=files)
            atomic_json(publish / "receipt.json", info)
            (publish / "receipt.json").chmod(0o644)  # Shared immutable cache metadata is not a credential.
            try:
                os.rename(publish, directory)
            except OSError:
                if not directory.is_dir():
                    raise
                # A competing publisher must have produced exactly this result.
                if directory.is_symlink() or binary.is_symlink() or digest(binary) != info["binary_sha256"]:
                    raise ValueError("conflicting cache publication") from None
            return {**info, "path": str(binary.resolve())}

    def stage(self, name, target, directory, offline=True):
        info = self.fetch(name, target, offline=offline)
        directory = Path(directory)
        # A new directory is the unit of ownership; never overwrite target files.
        directory.mkdir(mode=0o700, parents=False, exist_ok=False)
        try:
            files = info.get("files", {info["filename"]: info["binary_sha256"]})
            for filename, expected in files.items():
                if not NAME.fullmatch(filename):
                    raise ValueError("invalid cached filename")
                dest = directory / filename
                shutil.copyfile(Path(info["path"]).parent / filename, dest)
                dest.chmod(0o700 if filename == info["filename"] else 0o600)
                if digest(dest) != expected:
                    raise ValueError("staged artifact checksum mismatch")
            dest = directory / info["filename"]
            receipt = {**info, "path": str(dest.resolve()), "directory": str(directory.resolve())}
            atomic_json(directory / "receipt.json", receipt)
            return receipt
        except Exception:
            shutil.rmtree(directory)
            raise


def clean_stage(path):
    path = Path(path)
    if path.is_symlink() or path.parent.is_symlink():
        raise ValueError("refusing symlink receipt/directory")
    info = json.loads(path.read_text())
    directory = path.parent.resolve()
    if info.get("directory") != str(directory) or not NAME.fullmatch(info["filename"]):
        raise ValueError("receipt directory mismatch")
    files = info.get("files", {info["filename"]: info["binary_sha256"]})
    for filename, expected in files.items():
        if not NAME.fullmatch(filename):
            raise ValueError("invalid staged filename")
        binary = directory / filename
        if binary.is_symlink() or digest(binary) != expected:
            raise ValueError("staged file changed; refusing cleanup")
    if {p.name for p in directory.iterdir()} != set(files) | {path.name}:
        raise ValueError("directory contains unowned files")
    for filename in files:
        (directory / filename).unlink()
    path.unlink()
    directory.rmdir()
    return {"cleaned": str(directory)}
