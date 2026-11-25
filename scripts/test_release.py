"""Release contracts; all Git repositories and Docker responses are disposable fixtures."""

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import release


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q", "-b", "main")
        self.git("remote", "add", "origin", "https://github.com/ylang-ylang/neko-rooms.git")
        self.put("manager.go", "package main\n")
        self.put("Dockerfile", "FROM scratch\n")
        self.put(f"{release.BROWSER_CONTEXT}/Dockerfile",
                 "FROM example/chromium@sha256:" + "1" * 64 + "\nCOPY enable.py /tmp/enable.py\nCOPY relay.conf /etc/relay.conf\n")
        self.put(f"{release.BROWSER_CONTEXT}/enable.py", "print('enable')\n")
        self.put(f"{release.BROWSER_CONTEXT}/relay.conf", "relay=true\n")
        self.commit()

    def git(self, *args):
        return release.git(self.root, *args).decode().strip()

    def put(self, path, content):
        path = self.root / path
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def commit(self):
        files = [str(p.relative_to(self.root)) for p in self.root.rglob("*")
                 if p.is_file() and ".git" not in p.relative_to(self.root).parts]
        self.git("add", "--", *files)
        self.git("-c", "user.name=Release test", "-c", "user.email=test@example.invalid",
                 "commit", "-qm", "fixture")

    def plan(self):
        with patch.dict(release.os.environ, {"GITHUB_REPOSITORY": "", "DOCKERHUB_NAMESPACE": ""}):
            return release.make_plan(self.root)

    def test_manager_only_keeps_browser_tag_and_cache(self):
        before = self.plan()
        self.put("manager.go", "package changed\n")
        self.commit()
        after = self.plan()
        self.assertNotEqual(before["images"][0]["tag"], after["images"][0]["tag"])
        self.assertEqual(before["images"][1]["tag"], after["images"][1]["tag"])
        self.assertEqual(before["images"][0]["cache_ref"], after["images"][0]["cache_ref"])
        self.assertNotEqual(after["images"][0]["cache_scope"], after["images"][1]["cache_scope"])

    def test_readme_tests_and_uncommitted_files_do_not_change_browser_key(self):
        before = self.plan()
        self.put(f"{release.BROWSER_CONTEXT}/README.md", "Not a Docker COPY input\n")
        self.put(f"{release.BROWSER_CONTEXT}/test_adapter.py", "pass\n")
        self.put("docs/release.md", "documentation\n")
        self.commit()
        self.assertEqual(before["browser_key"], self.plan()["browser_key"])
        self.put(f"{release.BROWSER_CONTEXT}/enable.py", "uncommitted!\n")
        self.assertEqual(before["browser_key"], self.plan()["browser_key"])

    def test_adapter_content_changes_key(self):
        before = self.plan()
        self.put(f"{release.BROWSER_CONTEXT}/enable.py", "print('new adapter')\n")
        self.commit()
        self.assertNotEqual(before["browser_key"], self.plan()["browser_key"])

    def test_pinned_base_changes_key(self):
        before = self.plan()
        path = self.root / release.BROWSER_CONTEXT / "Dockerfile"
        path.write_text(path.read_text().replace("1" * 64, "2" * 64))
        self.commit()
        self.assertNotEqual(before["browser_key"], self.plan()["browser_key"])

    def test_file_mode_and_platform_are_inputs(self):
        before = self.plan()
        (self.root / release.BROWSER_CONTEXT / "enable.py").chmod(0o755)
        self.commit()
        self.assertNotEqual(before["browser_key"], self.plan()["browser_key"])
        inputs = release.browser_inputs(self.root, "HEAD")
        self.assertNotEqual(release.browser_key(inputs), release.browser_key(inputs, "linux/arm64"))

    def test_new_literal_copy_is_automatically_included(self):
        before = self.plan()
        path = self.root / release.BROWSER_CONTEXT / "Dockerfile"
        path.write_text(path.read_text() + "COPY extra.conf /etc/extra.conf\n")
        self.put(f"{release.BROWSER_CONTEXT}/extra.conf", "new input\n")
        self.commit()
        after = self.plan()
        self.assertIn("extra.conf", after["browser_inputs"])
        self.assertNotEqual(before["browser_key"], after["browser_key"])

    def test_missing_or_unsupported_copy_fails_instead_of_reusing_stale_image(self):
        path = self.root / release.BROWSER_CONTEXT / "Dockerfile"
        path.write_text("FROM scratch\nCOPY missing.conf /etc/input\n")
        self.commit()
        with self.assertRaisesRegex(ValueError, "missing"):
            self.plan()
        path.write_text("FROM scratch\nCOPY *.conf /etc/\n")
        self.commit()
        with self.assertRaisesRegex(ValueError, "Unsupported"):
            self.plan()

    def test_owned_namespace_is_explicit_not_github_owner(self):
        plan = self.plan()
        self.assertTrue(plan["images"][0]["tag"].startswith("docker.io/ylangylang/"))
        self.assertEqual(plan["source"], "https://github.com/ylang-ylang/neko-rooms")
        with self.assertRaisesRegex(ValueError, "namespace"):
            release.make_plan(self.root, repository="someone/another-fork", namespace="")
        plan = release.make_plan(self.root, repository="Someone/Fork", namespace="mydockeruser")
        self.assertEqual(plan["repository"], "someone/fork")
        self.assertTrue(plan["images"][1]["tag"].startswith("docker.io/mydockeruser/"))

    def test_key_encoding_is_stable(self):
        inputs = [("a", "100644", b"hello\n"), ("b", "100755", b"world\n")]
        # Golden key protects aliases already published with the v1 algorithm.
        self.assertEqual(release.browser_key(inputs),
                         "58509fc55eca2f09a5f58e03026d63f562671b9dd9f2f50c3aa624dc4a728453")


if __name__ == "__main__":
    unittest.main()
