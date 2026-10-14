#!/usr/bin/env python3
"""Generate retained, subtree-scoped NFS PV/PVC aliases; never read task files.

Input: the controller's existing PV JSON on stdin.
Output: Kubernetes List JSON for review/apply (no API mutation by this script).
Usage: kubectl get pv <controller-pv> -o json | python3 ci/shared-storage-manifest.py artex-runners
Directories must already exist and have compatible ownership before mounting.
"""
import json
import posixpath
import re
import sys

SCOPES = ("tasks", "sessions", "skills", "drafts", "runner-inputs")


def manifest(pv, namespace):
    if not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?", namespace):
        raise ValueError("invalid runner namespace")
    spec = pv["spec"]
    if "nfs" in spec:
        server, base = spec["nfs"]["server"], spec["nfs"]["path"]
    else:
        csi = spec["csi"]
        if csi["driver"] != "nfs.csi.k8s.io":
            raise ValueError("only explicit NFS sources are supported")
        attrs = csi["volumeAttributes"]
        server = attrs["server"]
        subdir = attrs.get("subdir", "")
        if subdir.startswith("/") or ".." in subdir.split("/"):
            raise ValueError("invalid source subdir")
        base = posixpath.join(attrs["share"], subdir)
    if not base.startswith("/") or ".." in base.split("/"):
        raise ValueError("invalid NFS export path")
    items = []
    for scope in SCOPES:
        name = "artex-shared-" + ("inputs" if scope == "runner-inputs" else scope)
        volume = namespace + "-" + name
        items.append({"apiVersion": "v1", "kind": "PersistentVolume", "metadata": {"name": volume, "labels": {"app.kubernetes.io/part-of": "artex-shared-storage"}}, "spec": {
            "capacity": spec["capacity"], "accessModes": ["ReadWriteMany"], "persistentVolumeReclaimPolicy": "Retain", "storageClassName": "", "mountOptions": spec.get("mountOptions", ["nfsvers=4.2"]),
            "claimRef": {"namespace": namespace, "name": name}, "nfs": {"server": server, "path": posixpath.join(base, scope)},
        }})
        items.append({"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"namespace": namespace, "name": name}, "spec": {
            "accessModes": ["ReadWriteMany"], "storageClassName": "", "volumeName": volume, "resources": {"requests": spec["capacity"]},
        }})
    return {"apiVersion": "v1", "kind": "List", "items": items}


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: shared-storage-manifest.py RUNNER_NAMESPACE < controller-pv.json")
    json.dump(manifest(json.load(sys.stdin), sys.argv[1]), sys.stdout, indent=2)
    print()
