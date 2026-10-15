#!/usr/bin/env python3
"""Execute Linux portable artifacts natively or with explicit user-mode QEMU.

Does not register binfmt handlers or require a privileged container. Foreign OS
artifacts remain marked not_run; compilation is not reported as execution.
"""
import argparse
import gzip
import json
from pathlib import Path
import platform
import resource
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
parser.add_argument("--directory", default="dist/portable-tools")
parser.add_argument("--qemu-directory", required=True)
args = parser.parse_args()
root = Path(args.directory)
report = json.loads((root / "build-report.json").read_text())
catalog = json.loads((root / "catalog.lock.json").read_text())
emulators = {"386": "i386", "amd64": "x86_64", "arm64": "aarch64", "armv5": "arm", "armv6": "arm", "armv7": "arm",
             "mips": "mips", "mipsle": "mipsel", "mips64": "mips64", "mips64le": "mips64el", "ppc64": "ppc64",
             "ppc64le": "ppc64le", "riscv64": "riscv64", "s390x": "s390x", "loong64": "loongarch64"}
failed = []
for name, tool in catalog["tools"].items():
    for target, artifact in tool["platforms"].items():
        if not target.startswith("linux/"):
            continue
        arch = target.split("/")[1]
        prefix = [] if arch in ("amd64", "386") and platform.machine() == "x86_64" else [str(Path(args.qemu_directory) / ("qemu-" + emulators[arch] + "-static"))]
        if arch in ("armv5", "armv6", "armv7"):
            prefix += ["-cpu", {"armv5": "arm926", "armv6": "arm1176", "armv7": "cortex-a9"}[arch]]
        try:
            with tempfile.TemporaryDirectory() as temp:
                binary = Path(temp) / name
                with gzip.open(root / artifact["file"], "rb") as source:
                    binary.write_bytes(source.read())
                binary.chmod(0o700)
                command = prefix + [str(binary), "-V" if name == "ffuf" else "--version"]
                result = subprocess.run(command, capture_output=True, text=True, timeout=45)
                if result.returncode or tool["version"] not in result.stdout + result.stderr:
                    raise ValueError(f"version failed ({result.returncode}): {(result.stdout + result.stderr)[-500:]}")
                if name == "yq":
                    result = subprocess.run(prefix + [str(binary), "-p=json", "-o=json", ".items | map(. + 1)"],
                                            input='{"items":[1,2,3]}', capture_output=True, text=True, timeout=45)
                    if result.returncode or json.loads(result.stdout) != [2, 3, 4]:
                        raise ValueError("real yq transformation failed")
            report["results"][name + ":" + target]["execution_test"] = "native_pass" if not prefix else "qemu_pass"
            print(name, target, "PASS", flush=True)
        except Exception as error:
            message = str(error)
            failed.append((name, target, message))
            report["results"][name + ":" + target]["execution_test"] = "failed"
            report["results"][name + ":" + target]["execution_error"] = message
            print(name, target, "FAIL", message, flush=True)
(root / "build-report.json").write_text(json.dumps(report, indent=2) + "\n")
if failed:
    raise SystemExit(f"{len(failed)} Linux runtime tests failed")
