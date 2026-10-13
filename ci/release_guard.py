#!/usr/bin/env python3
"""Fail closed before changing the artifacts of a published release."""
import json
import os
import re
import subprocess
import sys


def require_mutable_release(repo, tag):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise SystemExit("Invalid repository name")
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?", tag):
        raise SystemExit("Invalid release tag")
    existing = subprocess.run(
        ["gh", "api", f"repos/{repo}/releases/tags/{tag}"],
        capture_output=True, text=True,
    )
    if existing.returncode == 0:
        if not json.loads(existing.stdout)["draft"]:
            raise SystemExit(
                "This version is already published. Use promote.yml to retry latest promotion, "
                "or create a new version."
            )
        return
    try:
        status = str(json.loads(existing.stdout).get("status", ""))
    except ValueError:
        status = ""
    if status != "404":
        raise SystemExit("Cannot verify release immutability: " + existing.stderr)


if __name__ == "__main__":
    require_mutable_release(os.environ["GITHUB_REPOSITORY"], sys.argv[1])
