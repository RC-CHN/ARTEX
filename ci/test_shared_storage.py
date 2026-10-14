import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("shared_manifest", Path(__file__).with_name("shared-storage-manifest.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class SharedStorageManifestTest(unittest.TestCase):
    def test_aliases_preserve_existing_subtrees_and_retain_data(self):
        source = {"spec": {"capacity": {"storage": "10Gi"}, "csi": {"driver": "nfs.csi.k8s.io", "volumeAttributes": {"server": "fixture", "share": "/export", "subdir": "existing-pvc"}}}}
        items = module.manifest(source, "runners")["items"]
        self.assertEqual(len(items), 10)
        for pv, pvc in zip(items[::2], items[1::2]):
            self.assertEqual(pv["spec"]["persistentVolumeReclaimPolicy"], "Retain")
            self.assertTrue(pv["spec"]["nfs"]["path"].startswith("/export/existing-pvc/"))
            self.assertEqual(pvc["spec"]["volumeName"], pv["metadata"]["name"])
            self.assertEqual(pvc["spec"]["storageClassName"], "")
        source["spec"]["csi"]["volumeAttributes"]["subdir"] = "../other"
        with self.assertRaises(ValueError):
            module.manifest(source, "runners")
