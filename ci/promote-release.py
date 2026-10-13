#!/usr/bin/env python3
"""Promote only the highest complete stable release, under a workflow-wide lock."""
import json
import os
import re
import subprocess

tag = os.environ["RELEASE_TAG"]
repo = os.environ["GITHUB_REPOSITORY"]
if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag):
    raise SystemExit("Only stable vMAJOR.MINOR.PATCH tags can become latest")

def version(value):
    if re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", value):
        return tuple(map(int, value[1:].split(".")))
    return None

def complete(release):
    number = version(release["tag_name"])
    if number is None or release["draft"] or release["prerelease"]:
        return False
    stem = "artex-" + release["tag_name"][1:] + "-"
    expected = {stem + platform + ".zip" for platform in
                ["linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64"]}
    expected.add("SHA256SUMS")
    return expected <= {asset["name"] for asset in release["assets"]}

pages = json.loads(subprocess.check_output(
    ["gh", "api", "--paginate", "--slurp", f"repos/{repo}/releases?per_page=100"], text=True))
releases = [release for page in pages for release in page if complete(release)]
if not any(release["tag_name"] == tag for release in releases):
    raise SystemExit("Requested version is not a complete published stable release")
highest = max(releases, key=lambda release: version(release["tag_name"]))["tag_name"]
if highest != tag:
    print(f"Request for {tag} will align latest with newer complete release {highest}")
# GitHub concurrency keeps only one pending run by default. A later request for
# an older tag may replace the highest version's pending promotion; every valid
# request therefore converges on the highest complete version under the lock.
image = "ghcr.io/" + repo.lower()
# Update the container alias first. A failure leaves the release intact and
# can be retried through promote.yml without rewriting immutable versions.
subprocess.run(["docker", "buildx", "imagetools", "create", "--tag", image + ":latest", image + ":" + highest], check=True)
subprocess.run(["gh", "release", "edit", highest, "--repo", repo, "--latest=true"], check=True)
print(f"Promoted {highest} to GitHub and container latest")
