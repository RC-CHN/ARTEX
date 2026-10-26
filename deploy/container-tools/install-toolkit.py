#!/usr/bin/env python3
"""Install native pinned tools; foreign architectures stay in the catalog."""
import json
from pathlib import Path
import shutil
import sys

root = Path(sys.argv[2] if len(sys.argv) > 2 else "/")
sys.path.insert(0, str(root / "opt/artex-kit"))
from artex_toolkit.catalog import Catalog

catalog_path = root / "opt/artex-kit/catalog.lock.json"
manifest = json.loads(catalog_path.read_text())
portable = json.loads((root / "opt/artex-kit/portable/catalog.lock.json").read_text())
for name, tool in portable["tools"].items():
    official = manifest["tools"].setdefault(name, {"version": tool["version"], "platforms": {}})
    if official["version"] != tool["version"]:
        raise SystemExit("official/portable version mismatch: " + name)
    for platform, entry in tool["platforms"].items():
        official["platforms"][platform] = {**entry, "file": "portable/" + entry["file"]}
tack = json.loads((root / "opt/artex-kit/tack/catalog.lock.json").read_text())
for name, tool in tack["tools"].items():
    if name in manifest["tools"]:
        raise SystemExit("duplicate bundled tool: " + name)
    manifest["tools"][name] = {**tool, "platforms": {
        platform: {**entry, "file": "tack/" + entry["file"]}
        for platform, entry in tool["platforms"].items()}}
catalog_path.write_text(json.dumps(manifest, indent=2) + "\n")
catalog = Catalog(catalog_path, root / "opt/artex-kit/cache")
metadata = root / "opt/artex-tools/security-tools.lock.json"
lock = json.loads(metadata.read_text())
for name in catalog.data["tools"]:
    if catalog.data["tools"][name].get("native_install", True) is False:
        continue
    result = catalog.fetch(name, "linux/" + sys.argv[1])
    target = root / "usr/local/bin" / name
    target.parent.mkdir(parents=True, exist_ok=True)
    target.unlink(missing_ok=True)
    # Absolute within the image even when installing into a staging root.
    target.symlink_to("/" + str(Path(result["path"]).relative_to(root.resolve())))
    lock[name] = {"version": result["version"], "sha256": result["binary_sha256"]}
lock["artex-kit"] = {"version": "1.0.0"}
metadata.write_text(json.dumps(lock, indent=2) + "\n")
