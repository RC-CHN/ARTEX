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
            self.assertNotIn("nfs", pv["spec"])
            self.assertEqual(pv["spec"]["csi"]["driver"], "nfs.csi.k8s.io")
            self.assertTrue(pv["spec"]["csi"]["volumeAttributes"]["share"].startswith("/export/existing-pvc/"))
            self.assertEqual(pv["spec"]["csi"]["volumeAttributes"]["mountPermissions"], "0")
            self.assertEqual(pvc["spec"]["volumeName"], pv["metadata"]["name"])
            self.assertEqual(pvc["spec"]["storageClassName"], "")
        source["spec"]["csi"]["volumeAttributes"]["subdir"] = "../other"
        with self.assertRaises(ValueError):
            module.manifest(source, "runners")

    def test_case_insensitive_csi_subdir_and_unique_handles(self):
        source = {"spec": {"capacity": {"storage": "1Gi"}, "csi": {"driver": "nfs.csi.k8s.io", "volumeAttributes": {"server": "fixture", "share": "/export", "subDir": "existing"}}}}
        first = module.manifest(source, "runners")["items"][::2]
        second = module.manifest(source, "other")["items"][::2]
        self.assertEqual(first[0]["spec"]["csi"]["volumeAttributes"]["share"], "/export/existing/tasks")
        self.assertEqual(len({pv["spec"]["csi"]["volumeHandle"] for pv in first + second}), 10)

    def test_in_tree_source_migrates_to_csi_without_changing_data_path(self):
        source = {"spec": {"capacity": {"storage": "1Gi"}, "nfs": {"server": "fixture", "path": "/export/existing"}}}
        pv = module.manifest(source, "runners")["items"][0]
        self.assertEqual(pv["spec"]["csi"]["volumeAttributes"]["share"], "/export/existing/tasks")
