"""Exercise release guards without publishing or accessing external services."""
import contextlib
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest import mock

from release_guard import require_mutable_release


ROOT = Path(__file__).resolve().parent
REPO = "RC-CHN/ARTEX"


def response(status, body):
    return subprocess.CompletedProcess(["gh"], status, json.dumps(body), "API failed" if status else "")


def release(tag, *, draft=False, prerelease=False):
    names = ["artex-" + tag[1:] + "-" + platform + ".zip" for platform in
             ["linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64"]]
    return {"tag_name": tag, "draft": draft, "prerelease": prerelease,
            "assets": [{"name": name} for name in names + ["SHA256SUMS"]]}


class ReleaseGuardTests(unittest.TestCase):
    def test_unpublished_version_can_be_built_or_resumed(self):
        for result in [response(1, {"status": "404"}), response(0, {"draft": True})]:
            with self.subTest(result=result), mock.patch("subprocess.run", return_value=result) as run:
                require_mutable_release(REPO, "v0.4.0")
                self.assertEqual(run.call_args.args[0], ["gh", "api", "repos/RC-CHN/ARTEX/releases/tags/v0.4.0"])

    def test_specific_job_rerun_rechecks_publication(self):
        # Metadata passed while the release was absent. A successful job can be
        # rerun independently later, so its write-time gate must query again.
        with mock.patch("subprocess.run", side_effect=[response(1, {"status": "404"}), response(0, {"draft": False})]) as run:
            require_mutable_release(REPO, "v0.4.0")
            with self.assertRaisesRegex(SystemExit, "already published"):
                require_mutable_release(REPO, "v0.4.0")
            self.assertEqual(run.call_count, 2)

    def test_api_errors_do_not_authorize_artifact_writes(self):
        errors = [response(1, {"status": "403"}), response(1, {"status": "500"}),
                  subprocess.CompletedProcess(["gh"], 1, "", "connection failed")]
        for result in errors:
            with self.subTest(result=result), mock.patch("subprocess.run", return_value=result):
                with self.assertRaisesRegex(SystemExit, "Cannot verify"):
                    require_mutable_release(REPO, "v0.4.0")


class ReleaseMetadataTests(unittest.TestCase):
    def metadata(self, *, published=False, head="abc123", tag_head="abc123", ancestor=True):
        def check_output(args, **kwargs):
            return head if args[-1] == "HEAD" else tag_head

        def run(args, **kwargs):
            if args[0] == "git":
                self.assertEqual(args, ["git", "merge-base", "--is-ancestor", head, "origin/main"])
                if not ancestor:
                    raise subprocess.CalledProcessError(1, args)
                return subprocess.CompletedProcess(args, 0)
            return response(0, {"draft": False}) if published else response(1, {"status": "404"})

        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            with mock.patch.dict(os.environ, {"RELEASE_TAG": "v0.4.0", "GITHUB_REPOSITORY": REPO,
                                              "GITHUB_OUTPUT": str(output)}), \
                    mock.patch("subprocess.check_output", side_effect=check_output), \
                    mock.patch("subprocess.run", side_effect=run), contextlib.redirect_stdout(io.StringIO()):
                try:
                    runpy.run_path(str(ROOT / "release-metadata.py"), run_name="__main__")
                except (SystemExit, subprocess.CalledProcessError):
                    self.assertFalse(output.exists(), "rejected release exposed build outputs")
                    raise
            return dict(line.split("=", 1) for line in output.read_text().splitlines())

    def test_valid_tag_exposes_exact_validated_commit(self):
        self.assertEqual(self.metadata(), {"tag": "v0.4.0", "version": "0.4.0", "commit": "abc123",
                                          "image": "ghcr.io/rc-chn/artex", "prerelease": "false"})

    def test_published_version_is_rejected_before_build_outputs(self):
        with self.assertRaisesRegex(SystemExit, "already published"):
            self.metadata(published=True)

    def test_tag_checkout_mismatch_is_rejected(self):
        with self.assertRaisesRegex(SystemExit, "Checkout does not match"):
            self.metadata(tag_head="different")

    def test_tag_outside_main_is_rejected(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.metadata(ancestor=False)


class PromotionTests(unittest.TestCase):
    def promote(self, tag, releases, *, mutation_error=None):
        # Two pages ensure pagination is included in selection, not just the
        # first 100 releases. All commands are mocked, including Docker writes.
        pages = json.dumps([releases[:1], releases[1:]])
        with mock.patch.dict(os.environ, {"RELEASE_TAG": tag, "GITHUB_REPOSITORY": REPO}), \
                mock.patch("subprocess.check_output", return_value=pages), \
                mock.patch("subprocess.run", side_effect=mutation_error) as run, \
                contextlib.redirect_stdout(io.StringIO()):
            try:
                runpy.run_path(str(ROOT / "promote-release.py"), run_name="__main__")
            except SystemExit:
                run.assert_not_called()
                raise
        return [call.args[0] for call in run.call_args_list]

    def assert_promotes(self, calls, tag):
        self.assertEqual(calls, [
            ["docker", "buildx", "imagetools", "create", "--tag", "ghcr.io/rc-chn/artex:latest", "ghcr.io/rc-chn/artex:" + tag],
            ["gh", "release", "edit", tag, "--repo", REPO, "--latest=true"],
        ])

    def test_older_queued_request_replaces_and_finishes_highest_promotion(self):
        # v0.10.0 was pending behind another promotion, then a v0.9.0 request
        # replaced it in GitHub's single pending slot. The surviving request
        # must still promote v0.10.0, using semantic rather than lexical order.
        calls = self.promote("v0.9.0", [release("v0.9.0"), release("v0.10.0")])
        self.assert_promotes(calls, "v0.10.0")

    def test_only_complete_published_stable_versions_are_candidates(self):
        incomplete = release("v2.0.0")
        incomplete["assets"].pop()
        calls = self.promote("v0.4.0", [release("v0.4.0"), incomplete,
                                      release("v3.0.0", draft=True), release("v4.0.0-rc.1", prerelease=True)])
        self.assert_promotes(calls, "v0.4.0")

    def test_invalid_requests_cannot_trigger_promotion(self):
        for tag in ["v0.5.0", "v0.5.0-rc.1", "v0.4.0;bad"]:
            with self.subTest(tag=tag), self.assertRaises(SystemExit):
                self.promote(tag, [release("v0.4.0")])

    def test_github_failure_after_image_update_can_be_repaired(self):
        failure = subprocess.CalledProcessError(1, ["gh", "release", "edit"])
        with self.assertRaises(subprocess.CalledProcessError):
            self.promote("v0.4.0", [release("v0.4.0")],
                         mutation_error=[subprocess.CompletedProcess(["docker"], 0), failure])
        self.assert_promotes(self.promote("v0.4.0", [release("v0.4.0")]), "v0.4.0")

    def test_image_failure_does_not_advance_github_latest(self):
        with mock.patch.dict(os.environ, {"RELEASE_TAG": "v0.4.0", "GITHUB_REPOSITORY": REPO}), \
                mock.patch("subprocess.check_output", return_value=json.dumps([[release("v0.4.0")]])), \
                mock.patch("subprocess.run", side_effect=subprocess.CalledProcessError(1, ["docker"])) as run:
            with self.assertRaises(subprocess.CalledProcessError):
                runpy.run_path(str(ROOT / "promote-release.py"), run_name="__main__")
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[0][0], "docker")


if __name__ == "__main__":
    unittest.main()
