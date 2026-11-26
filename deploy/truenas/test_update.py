import copy
import importlib.util
import json
import os
import subprocess
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("tn_update", Path(__file__).with_name("update.py"))
update = importlib.util.module_from_spec(spec)
spec.loader.exec_module(update)


class UpdateTests(unittest.TestCase):
    def original(self):
        return {"services": {"neko": {"image": "legacy@sha256:old", "ports": ["28320:8080"],
                                    "volumes": ["/original/profile:/home/neko/profile"]},
                             "cdp-proxy": {"image": "legacy-proxy", "network_mode": "service:neko"},
                             "neko-rooms": {"image": "manager-old", "environment": {"MODE": "direct"}}},
                "configs": {"legacy": {"content": "original $$nginx_variable"}}}

    def rendered(self):
        return {"services": {"neko": {"image": "do-not-apply-template-default"}, "cdp-proxy": {},
                             "neko-rooms": {"image": "channel", "environment": {"MODE": "relay"}},
                             "room-image": {"image": "channel"}, "gateway": {"image": "gateway@sha256:pinned"}}}

    def test_normal_release_changes_only_manager_image(self):
        original = self.original()
        result = update.prepare_config(original, "manager-new", "standby")
        expected = copy.deepcopy(original)
        expected["services"]["neko-rooms"]["image"] = "manager-new"
        self.assertEqual(result, expected)
        self.assertEqual(original["services"]["neko-rooms"]["image"], "manager-old")

    def test_migration_preserves_exact_legacy_and_pins_new_images(self):
        original, rendered = self.original(), self.rendered()
        result = update.prepare_config(original, "manager-new", "browser-new", rendered)
        for name in update.LEGACY:
            self.assertEqual(result["services"][name], original["services"][name])
            self.assertIsNot(result["services"][name], original["services"][name])
        self.assertEqual(result["configs"], original["configs"])
        self.assertEqual(result["services"]["room-image"]["image"], "browser-new")
        self.assertEqual(result["services"]["neko-rooms"]["environment"]["NEKO_ROOMS_NEKO_IMAGES"], "browser-new")
        self.assertEqual(result["services"]["gateway"]["pull_policy"], "never")
        self.assertEqual(rendered["services"]["room-image"]["image"], "channel")

    def test_existing_native_topology_can_change_public_port(self):
        native = update.prepare_config(self.original(), "manager", "browser", self.rendered())
        native["services"]["gateway"]["ports"] = ["192.168.202.11:28322:443"]
        rendered = copy.deepcopy(native)
        rendered["services"]["gateway"]["ports"] = ["192.168.202.11:22222:443"]
        rendered["services"]["neko-rooms"]["environment"]["NEKO_ROOMS_INSTANCE_URL"] = "https://192.168.202.11:22222/"
        result = update.prepare_config(native, "manager", "browser", rendered)
        self.assertEqual(result["services"]["gateway"]["ports"], ["192.168.202.11:22222:443"])
        for name in update.LEGACY:
            self.assertEqual(result["services"][name], native["services"][name])
        self.assertEqual(native["services"]["gateway"]["ports"], ["192.168.202.11:28322:443"])

    def test_migration_rejects_unreviewed_services_and_missing_browser(self):
        original = self.original()
        original["services"]["another-browser"] = {"image": "protected"}
        with self.assertRaisesRegex(RuntimeError, "restart-scope"):
            update.prepare_config(original, "new", "browser", self.rendered())
        with self.assertRaisesRegex(ValueError, "browser image"):
            update.prepare_config(self.original(), "new", None, self.rendered())

    def rows(self):
        fields = {"image": "same", "ports": {"8080/tcp": []}, "mounts": [{"Destination": "/profile"}],
                  "id": "original", "started": "before", "pid": 123, "running": True, "paused": False}
        return [dict(fields, name="/ix-neko-neko-1", project="ix-neko"),
                dict(fields, name="/tn-neko-rooms-work", project=None)]

    def test_migration_can_remove_legacy_services_and_keep_external_rooms(self):
        original = self.original()
        rendered = {"services": {"neko-rooms": {"image": "channel", "environment": {"MODE": "relay"}},
                                 "room-image": {"image": "channel"}, "gateway": {"image": "gateway@sha256:pinned"}}}
        result = update.prepare_config(original, "manager-new", "browser-new", rendered)
        self.assertNotIn("neko", result["services"])
        self.assertNotIn("cdp-proxy", result["services"])
        self.assertNotIn("configs", result)
        self.assertEqual(result["services"]["room-image"]["image"], "browser-new")
        self.assertEqual(result["services"]["neko-rooms"]["image"], "manager-new")

        # Subsequent migration when original already has no legacy services also succeeds.
        again = update.prepare_config(result, "manager-v2", "browser-v2", rendered)
        self.assertEqual(again["services"]["neko-rooms"]["image"], "manager-v2")
        self.assertEqual(again["services"]["room-image"]["image"], "browser-v2")

    def test_default_removal_preserves_external_rooms_and_rejects_leftover_default(self):
        before = self.rows() + [dict(self.rows()[0], name="/ix-neko-cdp-proxy-1")]
        only_external = [copy.deepcopy(before[1])]
        self.assertEqual(update.assert_preserved(before, only_external, True, allow_default_removal=True),
                         ["/tn-neko-rooms-work"])
        # Zero external rooms after legacy removal is allowed when legacy containers are gone.
        self.assertEqual(update.assert_preserved([before[0]], [], True, allow_default_removal=True), [])
        with self.assertRaisesRegex(RuntimeError, "ix-neko-neko-1"):
            update.assert_preserved(before, copy.deepcopy(before[:2]), True, allow_default_removal=True)
        with self.assertRaisesRegex(RuntimeError, "ix-neko-cdp-proxy-1"):
            update.assert_preserved(before, [copy.deepcopy(before[1]), copy.deepcopy(before[2])], True,
                                    allow_default_removal=True)

    def test_only_authorized_default_identity_may_change(self):
        before = self.rows()
        after = copy.deepcopy(before)
        after[0].update(id="new", started="after", pid=456)
        with self.assertRaisesRegex(RuntimeError, "id"):
            update.assert_preserved(before, after)
        self.assertEqual(len(update.assert_preserved(before, after, True)), 2)
        after[1]["pid"] = 789
        with self.assertRaisesRegex(RuntimeError, "work pid"):
            update.assert_preserved(before, after, True)

    def test_default_image_ports_mounts_and_missing_rooms_are_fatal(self):
        before = self.rows()
        for field, value in (("image", "changed"), ("ports", {}), ("mounts", [])):
            after = copy.deepcopy(before)
            after[0][field] = value
            with self.assertRaisesRegex(RuntimeError, field):
                update.assert_preserved(before, after, True)
        with self.assertRaisesRegex(RuntimeError, "work"):
            update.assert_preserved(before, before[:1], True)
        with self.assertRaisesRegex(RuntimeError, "empty"):
            update.assert_preserved([], [])

    def test_api_error_never_automatically_recreates_app_again(self):
        calls = []
        original = self.original()
        class Client:
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def call(self, method, *args, **kwargs):
                calls.append(method)
                if method == "app.config": return original
                if method == "app.update": raise RuntimeError("simulated API failure")
                raise AssertionError(method)
        with tempfile.TemporaryDirectory() as directory, \
             mock.patch.object(update, "APPDATA", Path(directory)), \
             mock.patch.object(update.subprocess, "run", return_value=types.SimpleNamespace(returncode=0)), \
             mock.patch.dict(sys.modules, {"truenas_api_client": types.SimpleNamespace(Client=Client)}):
            with self.assertRaisesRegex(RuntimeError, "simulated API failure"):
                update.update("manager@sha256:new", "browser@sha256:existing")
        self.assertEqual(calls.count("app.update"), 1)

    def test_wrapper_forwards_topology_only_to_one_remote_update(self):
        wrapper = Path(__file__).with_name("publish-to-tn.sh")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "bin"
            binary.mkdir()
            log = root / "calls.log"
            scripts = {
                "python3": '#!/bin/bash\nprintf "PUB %s\\n" "$*" >> "$TEST_LOG"\nwhile (($#)); do if [[ $1 == --output ]]; then printf "{}" > "$2"; break; fi; shift; done\n',
                "ssh": '#!/bin/bash\nprintf "SSH %s\\n" "$*" >> "$TEST_LOG"\n',
                "scp": '#!/bin/bash\nprintf "SCP %s\\n" "$*" >> "$TEST_LOG"\n',
            }
            for name, source in scripts.items():
                (binary / name).write_text(source)
                (binary / name).chmod(0o700)
            env = dict(os.environ, PATH=str(binary) + ":" + os.environ["PATH"], TEST_LOG=str(log), TMPDIR=directory)
            env.pop("TN_SSH_CONTROL_PATH", None)
            result = subprocess.run(["bash", str(wrapper), "--compose", "/nas/candidate.json", "--baseline", "/nas/backup",
                                     "--namespace", "test"], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            calls = log.read_text().splitlines()
            publisher = next(line for line in calls if line.startswith("PUB "))
            self.assertIn("--namespace test", publisher)
            self.assertNotIn("--compose", publisher)
            updates = [line for line in calls if line.startswith("SSH ") and "sudo" in line]
            self.assertEqual(len(updates), 1)
            self.assertIn("--compose /nas/candidate.json --baseline /nas/backup", updates[0])
            self.assertFalse(list(root.glob("neko-publish-tn.*")))

    def test_wrapper_rejects_missing_baseline_before_publication(self):
        result = subprocess.run(["bash", str(Path(__file__).with_name("publish-to-tn.sh")), "--compose", "/nas/input"],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("requires --baseline", result.stderr)


if __name__ == "__main__":
    unittest.main()
