#!/usr/bin/env python3
"""Select a verified release's single Node binary from externally collected OS facts.

Run on the operator host. This tool never installs, executes or replaces a Node.
"""
import argparse
import json
from pathlib import Path
import re


def version_tuple(value):
    match = re.match(r"^(\d+)\.(\d+)(?:\.(\d+))?(?:[-+._].*)?$", value)
    if not match:
        raise ValueError("supply the target's numeric kernel/OS version")
    return tuple(int(v or 0) for v in match.groups())


def select(manifest, os_name, arch, kernel=None, windows_version=None, runtime="auto"):
    if manifest.get("format") != "tack-release-v2":
        raise ValueError("unsupported release manifest")
    aliases = {"x86_64": "amd64", "aarch64": "arm64", "i686": "386", "i386": "386",
               "armv5l": "armv5", "armv6l": "armv6", "armv7l": "armv7",
               "mips": "mips-softfloat", "mipsel": "mipsle-softfloat", "mipsle": "mipsle-softfloat",
               "mips64": "mips64-softfloat", "mips64el": "mips64le-softfloat", "mips64le": "mips64le-softfloat"}
    arch = aliases.get(arch, arch)
    if os_name == "linux":
        if not kernel:
            raise ValueError("Linux selection requires the target kernel version")
        kernel_version = version_tuple(kernel)
        floor = (3, 1, 0) if arch.startswith("armv") else (2, 6, 32)
        if arch == "mips64le-softfloat":
            floor = (4, 8, 0)
        if kernel_version < floor:
            raise ValueError("kernel is below this target's documented compatibility floor")
        chosen = ("legacy" if kernel_version < (3, 2, 0) else "modern") if runtime == "auto" else runtime
        if chosen == "modern" and kernel_version < (3, 2, 0):
            raise ValueError("modern Node requires Linux 3.2 or newer; select Legacy")
        if kernel_version < (3, 2, 0) and arch not in ("amd64", "386", "armv5", "armv6", "armv7"):
            raise ValueError("this architecture has no qualified sub-3.2 kernel path")
    elif os_name == "windows":
        if not windows_version or version_tuple(windows_version) < (10, 0, 0):
            raise ValueError("current Windows builds require Windows 10 / Server 2016 or newer; no Win7 fallback")
        if runtime == "legacy":
            raise ValueError("the Legacy runtime is a Linux compatibility build")
        chosen = "modern"
    else:
        raise ValueError("customer Node artifacts currently target Linux and Windows")

    matches = [entry for entry in manifest.get("artifacts", [])
               if entry.get("component") == "tack-node" and entry.get("os") == os_name
               and entry.get("arch") == arch and entry.get("build", {}).get("runtime_profile", "modern") == chosen]
    if len(matches) != 1:
        raise ValueError("release must contain exactly one matching Node; do not substitute another architecture or runtime")
    entry = matches[0]
    if not {"management", "embedded-chisel"}.issubset(entry["build"].get("capabilities", [])):
        raise ValueError("selected artifact is not a unified shell-and-tunnel client")
    if Path(entry["file"]).name != entry["file"]:
        raise ValueError("unsafe artifact filename")
    return {"file": entry["file"], "archive_sha256": entry["archive_sha256"],
            "tool": "tack-node-legacy" if chosen == "legacy" else "tack-node",
            "platform": os_name + "/" + arch.removesuffix("-softfloat"),
            "runtime_profile": chosen, "go_version": entry["build"]["go_version"],
            "protection": entry["build"]["protection"], "validation": entry["build"]["validation"],
            "note": "Preserve the same node configuration and complete state directory; foreground-check both capabilities before service replacement."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--os", choices=("linux", "windows"), required=True)
    parser.add_argument("--arch", required=True)
    parser.add_argument("--kernel")
    parser.add_argument("--windows-version", help="numeric Windows version, e.g. 10.0")
    parser.add_argument("--runtime", choices=("auto", "modern", "legacy"), default="auto")
    args = parser.parse_args()
    try:
        result = select(json.loads(args.manifest.read_text()), args.os, args.arch,
                        args.kernel, args.windows_version, args.runtime)
    except ValueError as error:
        parser.error(str(error))
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
