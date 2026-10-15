#!/usr/bin/env python3
"""Validate or merge portable build shards before caching or image import."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import shutil
import tempfile

ROOT = Path(__file__).resolve().parents[1]
MAX_BINARY = 64 * 1024 * 1024


def validate(directory, lock, expected=None):
    directory = Path(directory)
    if directory.is_symlink():
        raise ValueError("symlinks are not allowed in portable bundles")
    targets = set(lock["platforms"] if expected is None else expected)
    if not targets or not targets <= set(lock["platforms"]):
        raise ValueError("invalid platform selection")
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise ValueError("symlinks are not allowed in portable bundles")
    if json.loads((directory / "source.lock.json").read_text()) != lock:
        raise ValueError("portable source lock mismatch")
    catalog = json.loads((directory / "catalog.lock.json").read_text())
    report = json.loads((directory / "build-report.json").read_text())
    if catalog.get("schema") != 1 or set(catalog.get("tools", {})) != set(lock["tools"]):
        raise ValueError("portable tool set mismatch")
    if report.get("toolchain") != lock["go_version"] or set(report.get("platforms", [])) != targets:
        raise ValueError("portable toolchain or platform set mismatch")
    expected_results = {name + ":" + target for name in lock["tools"] for target in targets}
    if set(report.get("results", {})) != expected_results:
        raise ValueError("missing or unexpected build result")
    archive_names = set()
    for name, source in lock["tools"].items():
        tool = catalog["tools"][name]
        if tool.get("version") != source["version"] or set(tool.get("platforms", {})) != targets:
            raise ValueError("portable version or platform set mismatch")
        licenses = directory / "licenses" / name
        if not licenses.is_dir() or not any(p.is_file() and p.stat().st_size for p in licenses.iterdir()):
            raise ValueError("portable source license missing")
        for target, entry in tool["platforms"].items():
            os_name, arch = target.split("/")
            archive_name = f"{name}_{source['version']}_{os_name}_{arch}.gz"
            filename = name + (".exe" if os_name == "windows" else "")
            if entry.get("file") != "archives/" + archive_name or entry.get("format") != "gz" or entry.get("filename") != filename:
                raise ValueError("invalid portable artifact path or format")
            path = directory / "archives" / archive_name
            with path.open("rb") as archive:
                checksum = hashlib.file_digest(archive, "sha256").hexdigest()
            if path.stat().st_size > MAX_BINARY or checksum != entry.get("sha256"):
                raise ValueError("portable archive checksum mismatch")
            digest, size = hashlib.sha256(), 0
            with gzip.open(path, "rb") as binary:
                while chunk := binary.read(1024 * 1024):
                    size += len(chunk)
                    if size > MAX_BINARY:
                        raise ValueError("portable binary exceeds size limit")
                    digest.update(chunk)
            result = report["results"][name + ":" + target]
            if result.get("state") != "built" or result.get("binary_bytes") != size or result.get("archive_bytes") != path.stat().st_size or result.get("binary_sha256") != digest.hexdigest():
                raise ValueError("portable build result does not match archive")
            archive_names.add(archive_name)
    if {p.name for p in (directory / "archives").iterdir()} != archive_names:
        raise ValueError("unexpected portable archive")
    return catalog, report


def merge(shards, output, lock):
    output = Path(output)
    if output.exists():
        raise ValueError("merge output must not already exist")
    checked, seen = [], set()
    for shard in shards:
        shard = Path(shard)
        targets = json.loads((shard / "build-report.json").read_text())["platforms"]
        if len(targets) != len(set(targets)) or seen.intersection(targets):
            raise ValueError("duplicate portable platform")
        catalog, report = validate(shard, lock, targets)
        seen.update(targets)
        checked.append((shard, catalog, report))
    if seen != set(lock["platforms"]):
        raise ValueError("incomplete portable platform set")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="portable-merge-", dir=output.parent) as temp:
        assembled = Path(temp) / "bundle"
        (assembled / "archives").mkdir(parents=True)
        catalog = {"schema": 1, "tools": {name: {"version": tool["version"], "platforms": {}} for name, tool in lock["tools"].items()}}
        report = {"toolchain": lock["go_version"], "platforms": lock["platforms"], "results": {}}
        license_contents = None
        for shard, part, part_report in checked:
            licenses = {p.relative_to(shard / "licenses"): p.read_bytes() for p in (shard / "licenses").rglob("*") if p.is_file()}
            if license_contents is not None and licenses != license_contents:
                raise ValueError("portable source licenses differ between shards")
            license_contents = licenses
            for name, tool in part["tools"].items():
                catalog["tools"][name]["platforms"].update(tool["platforms"])
                for entry in tool["platforms"].values():
                    shutil.copyfile(shard / entry["file"], assembled / entry["file"])
            report["results"].update(part_report["results"])
        for path, content in license_contents.items():
            dest = assembled / "licenses" / path
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(content)
        for name, data in [("catalog.lock.json", catalog), ("source.lock.json", lock), ("build-report.json", report)]:
            (assembled / name).write_text(json.dumps(data, indent=2) + "\n")
        validate(assembled, lock)
        assembled.rename(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["validate", "merge"])
    parser.add_argument("directory", type=Path)
    parser.add_argument("shards", nargs="*", type=Path)
    parser.add_argument("--lock", type=Path, default=ROOT / "toolkit/portable-build.lock.json")
    args = parser.parse_args()
    source = json.loads(args.lock.read_text())
    if args.action == "validate":
        validate(args.directory, source)
    else:
        merge(args.shards, args.directory, source)
    print("Portable bundle verified:", args.directory)
