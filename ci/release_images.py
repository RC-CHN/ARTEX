#!/usr/bin/env python3
"""Carry native smoke results into publication using immutable image digests."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from release_guard import require_mutable_release


def validate_settings(image, commit, tag):
    if not re.fullmatch(r"ghcr\.io/[a-z0-9_.-]+/[a-z0-9_.-]+", image):
        raise ValueError("invalid image repository")
    if not re.fullmatch(r"[a-f0-9]{40}", commit):
        raise ValueError("invalid commit")
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?", tag):
        raise ValueError("invalid version tag")


def record(candidate, directory, *, image, commit, tag, arch):
    validate_settings(image, commit, tag)
    if arch not in {"amd64", "arm64"} or not re.fullmatch(re.escape(image) + r":build-[0-9]+-[0-9]+-" + re.escape(arch), candidate):
        raise ValueError("invalid native build candidate")
    digests = json.loads(subprocess.check_output(
        ["docker", "image", "inspect", candidate, "--format", "{{json .RepoDigests}}"], text=True))
    refs = [ref for ref in digests if re.fullmatch(re.escape(image) + r"@sha256:[a-f0-9]{64}", ref)]
    if len(refs) != 1:
        raise ValueError("expected exactly one pushed repository digest")
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    (directory / (arch + ".json")).write_text(json.dumps(
        {"arch": arch, "commit": commit, "tag": tag, "ref": refs[0]}, indent=2) + "\n")


def verified_refs(directory, *, image, commit, tag):
    validate_settings(image, commit, tag)
    directory = Path(directory)
    if {path.name for path in directory.iterdir()} != {"amd64.json", "arm64.json"}:
        raise ValueError("expected both native image records and no extra files")
    refs = {}
    for arch in ("amd64", "arm64"):
        record = json.loads((directory / (arch + ".json")).read_text())
        if record.get("arch") != arch or record.get("commit") != commit or record.get("tag") != tag:
            raise ValueError("native image does not match this release")
        ref = record.get("ref", "")
        if not isinstance(ref, str) or not re.fullmatch(re.escape(image) + r"@sha256:[a-f0-9]{64}", ref):
            raise ValueError("native image requires an immutable repository digest")
        refs[arch] = ref
    if refs["amd64"] == refs["arm64"]:
        raise ValueError("native architectures must have distinct image digests")
    return refs


def publish(directory, *, image, commit, tag):
    refs = verified_refs(directory, image=image, commit=commit, tag=tag)
    require_mutable_release(os.environ["GITHUB_REPOSITORY"], tag)
    for arch, ref in refs.items():
        subprocess.run(["docker", "buildx", "imagetools", "create", "--prefer-index=false",
                        "--tag", image + ":" + tag + "-" + arch, ref], check=True)
    subprocess.run(["docker", "buildx", "imagetools", "create", "--tag", image + ":" + tag,
                    "--tag", image + ":" + tag[1:], *refs.values()], check=True)


if __name__ == "__main__":
    settings = {key.lower(): os.environ[key] for key in ("IMAGE", "COMMIT", "TAG")}
    if len(sys.argv) == 4 and sys.argv[1] == "record":
        record(sys.argv[2], sys.argv[3], arch=os.environ["ARCH"], **settings)
    elif len(sys.argv) == 3 and sys.argv[1] == "publish":
        publish(sys.argv[2], **settings)
    else:
        raise SystemExit("usage: release_images.py record CANDIDATE DIRECTORY | publish DIRECTORY")
