#!/usr/bin/env python3
"""Install only checksum-pinned official CLI archives and Nuclei templates."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import zipfile

lock = json.loads(Path(__file__).with_name("security-tools.lock.json").read_text())
arch = sys.argv[1]
root = Path(sys.argv[2] if len(sys.argv) > 2 else "/")


def download(item, target):
    subprocess.run(["curl", "--fail", "--silent", "--show-error", "--location", "--retry", "3", "--retry-all-errors", "--connect-timeout", "30",
                    "--max-time", "600", "--output", str(target), item["url"]], check=True)
    with target.open("rb") as stream:
        actual = hashlib.file_digest(stream, "sha256").hexdigest()
    if actual != item["sha256"]:
        raise SystemExit(f"Checksum mismatch for {target.name}")


with tempfile.TemporaryDirectory() as tmp:
    tmp = Path(tmp)
    for name in ("vulnx", "nuclei"):
        archive = tmp / (name + ".zip")
        download(lock[name]["archives"][arch], archive)
        destination = root / ("usr/local/bin/vulnx" if name == "vulnx" else "usr/local/libexec/artex/nuclei")
        destination.parent.mkdir(parents=True, exist_ok=True)
        with zipfile.ZipFile(archive) as z:
            # Read the exact binary member, never extract arbitrary archive paths.
            with z.open(name) as source, destination.open("wb") as output:
                shutil.copyfileobj(source, output)
            for entry in z.namelist():
                if Path(entry).name.lower().startswith(("license", "notice")) and not entry.endswith("/"):
                    license_path = root / "usr/local/share/licenses" / name / Path(entry).name
                    license_path.parent.mkdir(parents=True, exist_ok=True)
                    license_path.write_bytes(z.read(entry))
        destination.chmod(0o755)
    archive = tmp / "templates.tar.gz"
    download(lock["nuclei-templates"], archive)
    unpacked = tmp / "templates"
    with tarfile.open(archive) as tar:
        tar.extractall(unpacked, filter="data")
    directory, = unpacked.iterdir()
    destination = root / "opt/nuclei-templates"
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.move(str(directory), destination)
    # Templates are a shared immutable image layer. Per-run outputs and config
    # live under the sandbox HOME/workspace, never in the packaged template tree.
    for current, dirs, files in os.walk(destination):
        Path(current).chmod(0o755)
        for filename in files:
            (Path(current) / filename).chmod(0o644)
    metadata = root / "opt/artex-tools/security-tools.lock.json"
    metadata.parent.mkdir(parents=True, exist_ok=True)
    metadata.write_text(json.dumps(lock, indent=2) + "\n")
