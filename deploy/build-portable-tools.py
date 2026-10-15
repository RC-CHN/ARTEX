#!/usr/bin/env python3
"""Build pinned small tools for every Linux target of the pinned Go toolchain.

Also builds supported Windows/macOS targets. Does not push or publish artifacts.
The output is an offline-importable catalog with compressed binaries and source
licenses. Failures are recorded and cause a nonzero exit; no fake platform rows.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default=str(ROOT / "dist/portable-tools"))
    parser.add_argument("--work", default="/tmp/artex-portable-build")
    parser.add_argument("--jobs", type=int, default=2)
    parser.add_argument("--platform", action="append", help="Subset for CI; default all supported Linux/Windows/macOS targets")
    parser.add_argument("--shards", type=int, default=1)
    parser.add_argument("--shard-index", type=int, default=0)
    args = parser.parse_args()
    lock = json.loads((ROOT / "toolkit/portable-build.lock.json").read_text())
    version = subprocess.check_output(["go", "env", "GOVERSION"], text=True).strip()
    if version != lock["go_version"]:
        raise SystemExit(f"need {lock['go_version']}, found {version}")
    supported = subprocess.check_output(["go", "tool", "dist", "list"], text=True).splitlines()
    targets = []
    for target in supported:
        if target.startswith(("linux/", "windows/", "darwin/")):
            targets.extend(["linux/armv5", "linux/armv6", "linux/armv7"] if target == "linux/arm" else [target])
    if set(targets) != set(lock["platforms"]):
        raise SystemExit("toolchain targets differ from locked platform list")
    targets = lock["platforms"]
    if args.platform:
        if set(args.platform) - set(targets):
            raise SystemExit("requested platform is not supported by this toolchain")
        targets = args.platform
    if not 1 <= args.shards <= len(targets) or not 0 <= args.shard_index < args.shards:
        raise SystemExit("invalid shard count or index")
    targets = targets[args.shard_index::args.shards]
    out, work = Path(args.output).resolve(), Path(args.work).resolve()
    (out / "archives").mkdir(parents=True, exist_ok=True)
    (out / "licenses").mkdir(exist_ok=True)
    (work / "sources").mkdir(parents=True, exist_ok=True)
    # Always compile a fresh extraction of the verified archive. A previous
    # work tree may have been edited; it is not a trusted source cache.
    with tempfile.TemporaryDirectory(prefix="verified-sources-", dir=work) as source_root:
        env = dict(os.environ, CGO_ENABLED="0", GOTOOLCHAIN="local", GOCACHE=str(work / "gocache"), GOMODCACHE=str(work / "gomodcache"))
        for name, tool in lock["tools"].items():
            archive = work / "sources" / (name + ".tar.gz")
            if not archive.exists() or hashlib.sha256(archive.read_bytes()).hexdigest() != tool["sha256"]:
                cached = Path(os.environ.get("ARTEX_TOOL_ARCHIVES", "/nonexistent")) / tool["sha256"]
                if cached.is_file():
                    shutil.copyfile(cached, archive)
                else:
                    with urllib.request.urlopen(tool["url"], timeout=120) as source, archive.open("wb") as target:
                        shutil.copyfileobj(source, target)
            if hashlib.sha256(archive.read_bytes()).hexdigest() != tool["sha256"]:
                raise SystemExit("source checksum mismatch: " + name)
            source_dir = Path(source_root) / name
            if not source_dir.exists():
                with tempfile.TemporaryDirectory(dir=work) as temp:
                    with tarfile.open(archive) as tar:
                        tar.extractall(temp, filter="data")
                    directory, = Path(temp).iterdir()
                    shutil.move(directory, source_dir)
            subprocess.run(["go", "mod", "download"], cwd=source_dir, env=env, check=True)
            subprocess.run(["go", "mod", "verify"], cwd=source_dir, env=env, check=True)
            for license_file in source_dir.iterdir():
                if license_file.is_file() and license_file.name.lower().startswith(("license", "notice")):
                    destination = out / "licenses" / name
                    destination.mkdir(exist_ok=True)
                    shutil.copyfile(license_file, destination / license_file.name)

        def build(item):
            name, target = item
            tool = lock["tools"][name]
            os_name, arch = target.split("/")
            build_env = dict(env, GOOS=os_name, GOARCH="arm" if arch.startswith("armv") else arch)
            if arch.startswith("armv"):
                build_env["GOARM"] = arch[-1] + ",softfloat"
            if arch in ("mips", "mipsle"):
                build_env["GOMIPS"] = "softfloat"
            if arch in ("mips64", "mips64le"):
                build_env["GOMIPS64"] = "softfloat"
            filename = name + (".exe" if os_name == "windows" else "")
            with tempfile.TemporaryDirectory(dir=work) as temp:
                binary = Path(temp) / filename
                started = time.monotonic()
                ldflags = "-s -w -buildid="
                if name == "chisel":
                    ldflags += " -X github.com/jpillora/chisel/share.BuildVersion=" + tool["version"]
                if name == "ffuf":
                    ldflags += " -X github.com/ffuf/ffuf/v2/pkg/ffuf.VERSION=" + tool["version"] + " -X github.com/ffuf/ffuf/v2/pkg/ffuf.VERSION_APPENDIX="
                command = ["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=" + ldflags, "-o", str(binary), tool["package"]]
                result = subprocess.run(command, cwd=Path(source_root) / name, env=build_env, capture_output=True, text=True)
                if result.returncode:
                    return name, target, None, {"state": "failed", "error": result.stderr[-3000:]}
                archive_name = f"{name}_{tool['version']}_{os_name}_{arch}.gz"
                archive = out / "archives" / archive_name
                with archive.open("wb") as f, gzip.GzipFile(fileobj=f, mode="wb", filename="", mtime=0, compresslevel=9) as z, binary.open("rb") as source:
                    shutil.copyfileobj(source, z)
                entry = {"file": "archives/" + archive_name, "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                         "format": "gz", "filename": filename,
                         "abi": f"{os_name}/{arch}; CGO_ENABLED=0; {version}" + ("; softfloat" if "armv" in arch or "mips" in arch else "")}
                record = {"state": "built", "seconds": round(time.monotonic() - started, 2), "binary_bytes": binary.stat().st_size,
                          "archive_bytes": archive.stat().st_size, "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                          "execution_test": "not_run"}
                return name, target, entry, record

        catalog = {"schema": 1, "tools": {name: {"version": tool["version"], "platforms": {}} for name, tool in lock["tools"].items()}}
        report = {"toolchain": version, "platforms": targets, "results": {}}
        def build_platform(target):
            # One compiler per target primes its standard-library cache; other
            # tools then reuse it. Independent targets run on separate workers.
            return [build((name, target)) for name in lock["tools"]]

        with ThreadPoolExecutor(max_workers=max(1, min(args.jobs, 4))) as pool:
            for results in pool.map(build_platform, targets):
                for name, target, entry, record in results:
                    if entry:
                        catalog["tools"][name]["platforms"][target] = entry
                    report["results"][name + ":" + target] = record
                    print(name, target, record["state"], flush=True)
                    (out / "catalog.lock.json").write_text(json.dumps(catalog, indent=2) + "\n")
                    (out / "build-report.json").write_text(json.dumps(report, indent=2) + "\n")
        shutil.copyfile(ROOT / "toolkit/portable-build.lock.json", out / "source.lock.json")
        if any(r["state"] != "built" for r in report["results"].values()):
            raise SystemExit("some targets failed; see build-report.json")



if __name__ == "__main__":
    main()
