#!/usr/bin/env python3
"""Prepare a verified IP data lock and archive cache; never changes a running Runner.

Run with the pinned runtime Python dependencies installed. No account is required.
Only an explicit invocation contacts upstreams. Install the resulting lock separately.
"""
import argparse
from datetime import datetime, timezone
import gzip
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "toolkit"))
from artex_toolkit.catalog import atomic_json, copy_bounded, digest
from artex_toolkit.ipdata import MAX_FILE, install


def download(url, path, limit=MAX_FILE):
    request = urllib.request.Request(url, headers={"User-Agent": "ARTEX-IP-Data-Updater/1.0"})
    with urllib.request.urlopen(request, timeout=60) as source, path.open("wb") as out:
        if not source.url.startswith("https://"):
            raise ValueError("upstream redirected away from HTTPS")
        copy_bounded(source, out, limit)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, help="Candidate lock file; existing file changes only on success")
    parser.add_argument("--archives", default=str(ROOT / "deploy/container-tools/download-cache"))
    args = parser.parse_args()
    cache = Path(args.archives)
    cache.mkdir(parents=True, exist_ok=True)
    manifest = json.loads((ROOT / "toolkit/ip-data.lock.json").read_text())
    with tempfile.TemporaryDirectory(prefix="artex-ip-refresh-") as temp:
        temp = Path(temp)
        download("https://api.github.com/repos/lionsoul2014/ip2region/commits/master", temp / "commit", 2 << 20)
        revision = json.loads((temp / "commit").read_text())["sha"]
        if not re.fullmatch(r"[a-f0-9]{40}", revision):
            raise ValueError("invalid ip2region revision")
        download("https://db-ip.com/db/download/ip-to-asn-lite", temp / "dbip", 2 << 20)
        releases = re.findall(r"https://download\.db-ip\.com/free/dbip-asn-lite-(\d{4}-\d{2})\.mmdb\.gz", (temp / "dbip").read_text())
        if not releases:
            raise ValueError("no published DB-IP ASN release")
        month = max(releases)
        for name, entry in manifest["sources"].items():
            entry["url"] = (f"https://download.db-ip.com/free/dbip-asn-lite-{month}.mmdb.gz" if name == "asn" else
                            f"https://raw.githubusercontent.com/lionsoul2014/ip2region/{revision}/data/ip2region_v{4 if name == 'ipv4' else 6}.xdb")
            archive, data = temp / name, temp / entry["filename"]
            download(entry["url"], archive)
            with (gzip.open(archive, "rb") if name == "asn" else archive.open("rb")) as source, data.open("wb") as out:
                copy_bounded(source, out, MAX_FILE)
            entry.update(sha256=digest(archive), size=archive.stat().st_size,
                         data_sha256=digest(data), data_size=data.stat().st_size)
            if name == "asn":
                entry["date"] = month + "-01"
            else:
                from ip2region import util
                with data.open("rb") as stream:
                    entry["date"] = datetime.fromtimestamp(util.load_header(stream).createdAt, timezone.utc).date().isoformat()
                entry["revision"] = revision
            target = cache / entry["sha256"]
            # Cache publication is atomic too; interrupted refreshes cannot
            # introduce partial archives consumed by a concurrent build.
            fd, tmp = tempfile.mkstemp(prefix=".download-", dir=cache)
            try:
                with os.fdopen(fd, "wb") as out, archive.open("rb") as source:
                    copy_bounded(source, out, MAX_FILE)
                os.chmod(tmp, 0o644)
                os.replace(tmp, target)
            finally:
                Path(tmp).unlink(missing_ok=True)
        manifest["release"] = datetime.now(timezone.utc).date().isoformat()
        candidate = temp / "candidate.json"
        atomic_json(candidate, manifest)
        install(candidate, temp / "verified", cache, offline=True)
        atomic_json(args.output, manifest)
        print(json.dumps({"manifest": args.output, "archives": str(cache), "sources": manifest["sources"]}, indent=2))


if __name__ == "__main__":
    main()
