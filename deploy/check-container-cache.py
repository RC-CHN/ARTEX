#!/usr/bin/env python3
"""Build two versions and verify that changing release metadata reuses tool layers."""

import argparse
import json
from pathlib import Path
import re
import subprocess
import time


def build(root, output, tag, version):
    started = time.monotonic()
    print(f"Building {version}; live log: {output}", flush=True)
    # Write every line as it arrives, including during slow image/browser
    # downloads. Preserve partial output when the builder disconnects or fails.
    with output.open("w", buffering=1) as log:
        with subprocess.Popen(
            [
                "docker", "build", "--progress=plain", "--tag", tag,
                "--build-arg", f"ARTEX_BUILD_VERSION={version}",
                "--build-arg", f"VCS_REF={version}", ".",
            ],
            cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, bufsize=1,
        ) as process:
            for line in process.stdout:
                log.write(line)
            returncode = process.wait()
    if returncode:
        raise SystemExit(f"Build failed; see {output}")
    elapsed = time.monotonic() - started
    print(f"Built {version} in {elapsed:.2f}s", flush=True)
    return elapsed, output.read_text()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-prefix", default="artex-cache-check")
    parser.add_argument("--output-dir", default="dist/container-cache-check")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = root / args.output_dir
    output.mkdir(parents=True, exist_ok=True)

    first, _ = build(root, output / "before.log", f"{args.image_prefix}:before", "cache-before")
    second, log = build(root, output / "after.log", f"{args.image_prefix}:after", "cache-after")
    steps = {}
    cached = set()
    for line in log.splitlines():
        match = re.match(r"^#(\d+) \[(runtime-tools|web|gobuild) [^]]+\] (RUN .+)$", line)
        if match:
            step, stage, instruction = match.groups()
            if stage == "runtime-tools" or "npm ci" in instruction or "go mod download" in instruction:
                steps[step] = f"{stage}: {instruction}"
        match = re.match(r"^#(\d+) CACHED$", line)
        if match:
            cached.add(match.group(1))

    if not any(value.startswith("runtime-tools:") for value in steps.values()):
        raise SystemExit(f"No runtime tool steps found; inspect {output / 'after.log'}")
    uncached = [name for step, name in steps.items() if step not in cached]
    result = {
        "before_seconds": round(first, 2),
        "after_seconds": round(second, 2),
        "checked_dependency_steps": len(steps),
        "uncached_dependency_steps": uncached,
    }
    (output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    if uncached:
        raise SystemExit("Release metadata invalidated dependency layers")


if __name__ == "__main__":
    main()
