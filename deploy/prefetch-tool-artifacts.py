#!/usr/bin/env python3
"""Prefetch public pinned archives without passing host proxy credentials to builds."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--platform", action="append", default=[])
parser.add_argument("--directory", default=str(ROOT / "deploy/container-tools/download-cache"))
parser.add_argument("--mirror-prefix", default="", help="Optional HTTPS prefix for public GitHub release URLs; hashes stay authoritative")
args = parser.parse_args()
if args.mirror_prefix:
    parsed = urllib.parse.urlsplit(args.mirror_prefix)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise SystemExit("mirror prefix must be a credential-free HTTPS URL")
platforms = args.platform or ["linux/amd64"]
directory = Path(args.directory)
directory.mkdir(parents=True, exist_ok=True)
entries = {}
ip_data = json.loads((ROOT / "toolkit/ip-data.lock.json").read_text())
for name, source in ip_data["sources"].items():
    entries[source["sha256"]] = ("IP data " + name, source)
catalog = json.loads((ROOT / "toolkit/catalog.lock.json").read_text())
for name, tool in catalog["tools"].items():
    for platform in platforms:
        entry = tool["platforms"][platform]
        entries[entry["sha256"]] = (name + " " + platform, entry)
security = json.loads((ROOT / "deploy/container-tools/security-tools.lock.json").read_text())
portable = json.loads((ROOT / "toolkit/portable-build.lock.json").read_text())
for name, source in portable["tools"].items():
    entries[source["sha256"]] = (name + " source", source)
for name, tool in security.items():
    if "archives" in tool:
        for platform in platforms:
            if platform.startswith("linux/"):
                entry = tool["archives"][platform.split("/")[1]]
                entries[entry["sha256"]] = (name + " " + platform, entry)
    elif "url" in tool:
        entries[tool["sha256"]] = (name, tool)


def fetch(pair):
    checksum, (label, item) = pair
    dest = directory / checksum
    if dest.exists() and hashlib.sha256(dest.read_bytes()).hexdigest() == checksum:
        return label + " cached"
    tmp = directory / (".partial-" + checksum)
    url = args.mirror_prefix + item["url"] if args.mirror_prefix and item["url"].startswith("https://github.com/") else item["url"]
    for attempt in range(4):
        result = subprocess.run(["curl", "-fsSL", "--continue-at", "-", "--connect-timeout", "20",
                                 "--max-time", "180", "--max-filesize", str(256 * 1024 * 1024), "-o", str(tmp), url])
        if tmp.exists() and hashlib.sha256(tmp.read_bytes()).hexdigest() == checksum:
            tmp.replace(dest)
            dest.chmod(0o644)
            return label + " verified"
        if result.returncode == 0:
            tmp.unlink(missing_ok=True)
            raise ValueError("checksum mismatch: " + label)
        if result.returncode == 33:  # Server does not support byte-range resume.
            tmp.unlink(missing_ok=True)
    raise ValueError("download interrupted; resumable partial retained for " + label)


with ThreadPoolExecutor(max_workers=4) as pool:
    for result in pool.map(fetch, entries.items()):
        print(result, flush=True)
