"""Release contracts; all Git repositories and Docker responses are disposable fixtures."""

import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import release


class PlanTests(unittest.TestCase):
    def setUp(self):
        environment = patch.dict(release.os.environ, {"GITHUB_REPOSITORY": "", "DOCKERHUB_NAMESPACE": ""})
        environment.start()
        self.addCleanup(environment.stop)
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

    def test_manager_only_plan_does_not_build_or_pull_existing_browser(self):
        browser_tag = self.plan()["images"][1]["tag"]
        self.put("manager.go", "package changed\n")
        self.commit()
        plan = self.plan()
        with patch.object(release, "remote_digest", side_effect=lambda tag: "sha256:" + "b" * 64 if tag == browser_tag else None), \
                patch.object(release.subprocess, "run") as run:
            release.probe_plan(plan)
            self.assertTrue(plan["images"][0]["build"])
            self.assertFalse(plan["images"][1]["build"])
            release.build_image(plan, plan["images"][1], self.root, "test-builder")
            run.assert_not_called()

    def test_first_publish_builds_missing_browser_and_rerun_preserves_immutable_manager(self):
        with patch.object(release, "remote_digest", side_effect=["sha256:" + "a" * 64, None]):
            plan = release.probe_plan(self.plan())
        self.assertFalse(plan["images"][0]["build"])
        self.assertTrue(plan["images"][1]["build"])
        with patch.object(release, "remote_digest", return_value="sha256:" + "a" * 64), \
                patch.object(release.subprocess, "run") as run:
            release.build_image(plan, plan["images"][0], self.root, "test-builder")
            run.assert_not_called()

    def test_build_command_has_only_immutable_tag_and_scoped_remote_cache(self):
        plan = self.plan()
        for image in plan["images"]:
            command = release.build_command(plan, image, self.root, "test-builder")
            self.assertIn(image["tag"], command)
            self.assertNotIn(image["channel"], command)
            self.assertIn("linux/amd64", command)
            self.assertIn(f"type=registry,ref={image['cache_ref']},mode=max", command)
            self.assertEqual(command[-1], str(self.root / image["context"]))

    def test_snapshot_excludes_dirty_source_untracked_files_and_git_metadata(self):
        self.put("manager.go", "dirty source\n")
        self.put("private.env", "DO_NOT_BUILD\n")
        with release.snapshot(self.root, "HEAD") as source:
            self.assertEqual((source / "manager.go").read_text(), "package main\n")
            self.assertFalse((source / "private.env").exists())
            self.assertFalse((source / ".git").exists())

    def test_github_step_outputs_preserve_labels_and_boolean_build_flag(self):
        with patch.object(release, "remote_digest", return_value=None):
            plan = release.probe_plan(self.plan())
        plan["images"] = plan["images"][:1]
        output = self.root / "outputs"
        release.emit_plan(plan, self.root / "plan.json", output)
        fields = {}
        lines = iter(output.read_text().splitlines())
        for line in lines:
            key, delimiter = line.split("<<", 1)
            value = []
            for content in lines:
                if content == delimiter:
                    break
                value.append(content)
            fields[key] = "\n".join(value)
        self.assertEqual(fields["build"], "true")
        self.assertEqual(fields["tag"], plan["images"][0]["tag"])
        self.assertEqual(fields["labels"], plan["images"][0]["labels"])
        self.assertEqual(fields["platform"], "linux/amd64")

    def test_missing_artifact_prevents_any_channel_write(self):
        plan = self.plan()
        with patch.object(release, "remote_digest", side_effect=["sha256:" + "a" * 64, None]), \
                patch.object(release.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "missing immutable"):
                release.promote(self.root, plan, self.root / "release.json")
            run.assert_not_called()

    def test_promotion_records_and_promotes_exact_pair_digests(self):
        manager = "sha256:" + "a" * 64
        browser = "sha256:" + "b" * 64
        plan = self.plan()
        output = self.root / "release.json"
        with patch.object(release, "remote_digest", side_effect=[manager, browser]), \
                patch.object(release.subprocess, "run") as run, patch("builtins.print"):
            release.promote(self.root, plan, output)
        manifest = json.loads(output.read_text())
        self.assertTrue(manifest["channels_promoted"])
        self.assertEqual(manifest["images"]["browser"]["digest"], browser)
        self.assertEqual(manifest["images"]["manager"]["pinned"], plan["images"][0]["image"] + "@" + manager)
        self.assertEqual(run.call_count, 2)
        for call, image in zip(run.call_args_list, plan["images"]):
            self.assertIn("--prefer-index=false", call.args[0])
            self.assertIn(image["channel"], call.args[0])
        self.assertIn(manifest["images"]["browser"]["pinned"], release.summary(manifest))

    def test_failed_channel_write_leaves_manifest_unpromoted(self):
        output = self.root / "release.json"
        plan = self.plan()
        with patch.object(release, "remote_digest", return_value="sha256:" + "a" * 64), \
                patch.object(release.subprocess, "run", side_effect=subprocess.CalledProcessError(1, ["docker"])):
            with self.assertRaises(subprocess.CalledProcessError):
                release.promote(self.root, plan, output)
        self.assertFalse(json.loads(output.read_text())["channels_promoted"])

    def test_old_branch_tip_prevents_channel_write(self):
        plan = self.plan()
        with patch.object(release, "remote_digest", return_value="sha256:" + "a" * 64), \
                patch.object(release, "git", return_value=b"older-sha\trefs/heads/main\n"), \
                patch.object(release.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "stale promotion"):
                release.promote(self.root, plan, self.root / "release.json", expected_ref="refs/heads/main")
            run.assert_not_called()

    def publish_args(self, dry_run=False):
        return SimpleNamespace(dry_run=dry_run, output=self.root / "release.json", builder="test-builder",
                               summary=None, expected_ref=None)

    def test_manual_dry_run_is_offline_and_has_no_build_side_effects(self):
        plan = self.plan()
        with patch.object(release, "remote_digest") as remote, patch.object(release.subprocess, "run") as run:
            release.publish(self.root, plan, self.publish_args(dry_run=True))
            remote.assert_not_called()
            run.assert_not_called()
        self.assertTrue(json.loads((self.root / "release.json").read_text())["dry_run"])

    def test_manual_publish_runs_no_local_component_checks(self):
        # CI (build.yml) gates the Go/frontend/adapter checks; the manual
        # publisher never needs host Go, Node or npm, only Git/tar and Docker.
        plan = self.plan()
        with patch.object(release, "remote_digest", return_value="sha256:" + "a" * 64), \
                patch.object(release.subprocess, "run") as run, \
                patch.object(release, "build_image") as build, patch.object(release, "promote") as promote:
            release.publish(self.root, plan, self.publish_args())
            build.assert_not_called()
            promote.assert_called_once()
        for call in run.call_args_list:
            command = call.args[0]
            self.assertNotIn(command[0], ("go", "npm", "node"))
            self.assertNotIn("unittest", command)

    def test_manual_failed_image_build_never_promotes(self):
        plan = self.plan()
        with patch.object(release, "remote_digest", return_value=None), \
                patch.object(release, "snapshot") as snapshot, patch.object(release.subprocess, "run"), \
                patch.object(release, "ensure_builder"), \
                patch.object(release, "build_image", side_effect=RuntimeError("build failed")), \
                patch.object(release, "promote") as promote:
            snapshot.return_value.__enter__.return_value = self.root
            with self.assertRaisesRegex(RuntimeError, "build failed"):
                release.publish(self.root, plan, self.publish_args())
            promote.assert_not_called()

    def test_manual_publisher_skips_large_existing_browser(self):
        plan = self.plan()
        browser_tag = plan["images"][1]["tag"]
        with patch.object(release, "remote_digest", side_effect=lambda tag: "sha256:" + "b" * 64 if tag == browser_tag else None), \
                patch.object(release, "snapshot") as snapshot, patch.object(release.subprocess, "run"), \
                patch.object(release, "ensure_builder"), \
                patch.object(release, "build_image") as build, patch.object(release, "promote"):
            snapshot.return_value.__enter__.return_value = self.root
            release.publish(self.root, plan, self.publish_args())
        self.assertEqual(build.call_count, 1)
        self.assertEqual(build.call_args.args[1]["name"], "manager")

    def test_actual_cli_manager_only_change_plan_reuses_browser_manifest(self):
        before = self.plan()
        browser_tag = before["images"][1]["tag"]
        self.put("manager.go", "package changed\n")
        self.commit()
        fakebin = self.root / "fakebin"
        fakebin.mkdir()
        docker = fakebin / "docker"
        docker.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
with open(os.environ["DOCKER_TRACE"], "a") as trace:
    trace.write(json.dumps(args) + "\\n")
if args[:3] != ["buildx", "imagetools", "inspect"]:
    sys.exit("unexpected Docker operation")
if args[3] == os.environ["EXISTING_BROWSER"]:
    print(json.dumps({"digest": "sha256:" + "b" * 64}))
else:
    print("manifest unknown", file=sys.stderr)
    sys.exit(1)
''')
        docker.chmod(0o755)
        trace = self.root / "docker-trace.jsonl"
        environment = dict(release.os.environ, PATH=str(fakebin) + ":" + release.os.environ["PATH"],
                           DOCKER_TRACE=str(trace), EXISTING_BROWSER=browser_tag)
        output = subprocess.check_output([release.sys.executable, "-B", str(Path(release.__file__).resolve()),
                                          "plan", "--probe"], cwd=self.root, env=environment, text=True)
        after = json.loads(output)
        self.assertTrue(after["images"][0]["build"])
        self.assertFalse(after["images"][1]["build"])
        self.assertEqual(after["images"][1]["tag"], browser_tag)
        self.assertNotEqual(before["images"][0]["tag"], after["images"][0]["tag"])
        operations = [json.loads(line) for line in trace.read_text().splitlines()]
        self.assertEqual(len(operations), 2)
        self.assertTrue(all(op[:3] == ["buildx", "imagetools", "inspect"] for op in operations))


class RegistryTests(unittest.TestCase):
    def test_manifest_probe_returns_digest_without_layer_pull(self):
        digest = "sha256:" + "a" * 64
        with patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps({"digest": digest}), "")) as run:
            self.assertEqual(release.remote_digest("example/image:tag"), digest)
        command = run.call_args.args[0]
        self.assertEqual(command[:4], ["docker", "buildx", "imagetools", "inspect"])
        self.assertNotIn("pull", command)

    def test_missing_tag_triggers_build_but_auth_and_network_errors_never_do(self):
        for error in ("manifest unknown", "ERROR: example/image:tag: not found", "404 Not Found", "unexpected status: 404"):
            with self.subTest(error=error), patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", error)):
                self.assertIsNone(release.remote_digest("example/image:tag"))
        for error in ("unauthorized", "denied: repository not found", "403 Forbidden", "insufficient_scope",
                      "429 Too Many Requests", "timeout", "no such host", "connection reset by peer"):
            with self.subTest(error=error), patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", error)):
                with self.assertRaisesRegex(RuntimeError, "probe failed"):
                    release.remote_digest("example/image:tag")

    def test_invalid_digest_is_not_accepted_as_reusable(self):
        with patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, '{"digest":"invalid"}', "")):
            with self.assertRaisesRegex(ValueError, "invalid digest"):
                release.remote_digest("example/image:tag")


class BuildBoundaryTests(unittest.TestCase):
    def test_dependency_layers_precede_source_copy_and_no_workflow_dockerfile_rewrite(self):
        root = Path(release.__file__).resolve().parent.parent
        dockerfile = (root / "Dockerfile").read_text()
        self.assertLess(dockerfile.index("COPY client/package.json"), dockerfile.index("npm ci"))
        self.assertLess(dockerfile.index("npm ci"), dockerfile.index("COPY client/ ."))
        self.assertLess(dockerfile.index("COPY go.mod go.sum"), dockerfile.index("go mod download"))
        self.assertLess(dockerfile.index("go mod download"), dockerfile.index("COPY cmd/"))
        self.assertNotIn("COPY . .", dockerfile)
        workflow = (root / ".github/workflows/build.yml").read_text()
        self.assertNotIn("sed -i", workflow)
        self.assertIn("needs: [go, frontend, images]", workflow)
        self.assertNotIn("    outputs:", workflow)
        self.assertNotIn("linux/arm", workflow)
        self.assertNotIn("GHCR_ACCESS_TOKEN", workflow)


if __name__ == "__main__":
    unittest.main()
