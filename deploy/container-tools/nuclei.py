#!/usr/bin/env python3
"""Use bundled templates with a fresh non-root HOME; leave networking untouched."""
import json
import os
from pathlib import Path
import sys
import tempfile

config = Path(os.environ.setdefault("NUCLEI_CONFIG_DIR", str(Path.home() / ".config/nuclei")))
config.mkdir(parents=True, exist_ok=True)
target = config / ".templates-config.json"
if not target.exists():
    version = json.loads(Path("/opt/artex-tools/security-tools.lock.json").read_text())["nuclei-templates"]["version"]
    # Link a complete temporary file into place atomically. Parallel calls in the
    # same sandbox cannot observe a partially written first-run configuration.
    with tempfile.NamedTemporaryFile(mode="w", dir=config, delete=False) as stream:
        json.dump({"nuclei-templates-directory": "/opt/nuclei-templates", "nuclei-templates-version": "v" + version}, stream)
        temporary = stream.name
    try:
        try:
            os.link(temporary, target)
        except FileExistsError:
            pass
    finally:
        os.unlink(temporary)

args = sys.argv[1:]
explicit_update = any(arg.split("=", 1)[0] in ("-ut", "-update-templates", "-up", "-update") for arg in args)
defaults = [] if explicit_update else ["-disable-update-check"]
binary = "/usr/local/libexec/artex/nuclei"
os.execv(binary, [binary, *defaults, *args])
