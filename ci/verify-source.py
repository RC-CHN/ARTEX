#!/usr/bin/env python3
"""Keep active installation, build and update paths independent of old publishers."""
import json
import re
import subprocess
from pathlib import Path

root = Path(__file__).resolve().parents[1]
files = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard"], cwd=root, text=True).splitlines()
legacy = re.compile(r"(?:github\.com/)?(?:Autumn-27/(?:artex|norma)|mhtsec/ARTEX)|(?:autumn27|mianhuatang11)/artex|artex-demo\.mhtsec\.com", re.I)
errors = []
for name in files:
    path = root / name
    if path.suffix not in {".go", ".mod", ".sum", ".sh", ".bat", ".yml", ".yaml", ".json", ".md", ".ts", ".tsx", ".js", ".cjs", ".mjs"} and name not in {"Dockerfile", ".env.example"}:
        continue
    # Preserve historical measurements and original authorship, not live endpoints.
    if name in {"NOTICE.md"} or name.startswith("benchmarks/"):
        continue
    text = path.read_text()
    for number, line in enumerate(text.splitlines(), 1):
        if legacy.search(line):
            errors.append(f"{name}:{number}: obsolete source or publisher")
        if "@latest" in line:
            errors.append(f"{name}:{number}: floating tool version")
    if name.startswith(".github/workflows/"):
        for match in re.finditer(r"uses:\s*([^\s#]+)", text):
            action = match.group(1)
            if not action.startswith("./") and not re.fullmatch(r"[^@]+@[0-9a-f]{40}", action):
                errors.append(f"{name}: action is not pinned: {action}")

for directory in ["deploy/container-tools", "skills/api-recon/scripts"]:
    package = json.loads((root / directory / "package.json").read_text())
    lock = json.loads((root / directory / "package-lock.json").read_text())
    for name, version in package.get("dependencies", {}).items():
        if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?", version):
            errors.append(f"{directory}: {name} must have an exact version")
        if lock["packages"][""]["dependencies"].get(name) != version:
            errors.append(f"{directory}: lockfile does not match {name}")
for module in ["go.mod", "norma/go.mod"]:
    expected = "github.com/RC-CHN/ARTEX" + ("/norma" if module.startswith("norma/") else "")
    if not (root / module).read_text().startswith("module " + expected + "\n"):
        errors.append(f"{module}: module must be owned by this repository")
if errors:
    raise SystemExit("\n".join(errors))
print("Source ownership, pinned tools and workflow actions verified")
