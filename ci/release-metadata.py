#!/usr/bin/env python3
"""Validate a release tag and expose only validated values to Actions jobs."""
import os
import re
import subprocess
from pathlib import Path

from release_guard import require_mutable_release

tag = os.environ["RELEASE_TAG"]
if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?", tag):
    raise SystemExit("Release tag must be vMAJOR.MINOR.PATCH, optionally with a prerelease suffix")
repo = os.environ["GITHUB_REPOSITORY"]
if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
    raise SystemExit("Invalid repository name")
commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
tag_commit = subprocess.check_output(["git", "rev-parse", f"refs/tags/{tag}^{{commit}}"], text=True).strip()
if commit != tag_commit:
    raise SystemExit("Checkout does not match the requested tag")
subprocess.run(["git", "merge-base", "--is-ancestor", commit, "origin/main"], check=True)
# Reject immutable public versions before any architecture image can be pushed.
require_mutable_release(repo, tag)
values = {"tag": tag, "version": tag[1:], "commit": commit,
          "image": "ghcr.io/" + repo.lower(), "prerelease": str("-" in tag).lower()}
with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
    for key, value in values.items():
        output.write(f"{key}={value}\n")
print(f"Validated {tag} at {commit}")
