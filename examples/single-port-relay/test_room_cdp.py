#!/usr/bin/env python3
"""Build-time adapter contracts; no Docker daemon or browser needed."""
import configparser
import importlib.util
import json
from pathlib import Path
import unittest

HERE = Path(__file__).resolve().parent
ADAPTER = HERE / "room-cdp"
spec = importlib.util.spec_from_file_location("enable_room_cdp", ADAPTER / "enable.py")
enable_cdp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(enable_cdp)

# From the exact official image pinned by room-cdp/Dockerfile.
NATIVE_CHROMIUM = """[program:chromium]
environment=HOME="/home/%(ENV_USER)s",USER="%(ENV_USER)s",DISPLAY="%(ENV_DISPLAY)s"
command=/usr/bin/chromium
  --no-sandbox
  --window-position=0,0
  --display=%(ENV_DISPLAY)s
  --user-data-dir=/home/neko/.config/chromium
  --no-first-run
  --start-maximized
  --bwsi
  --force-dark-mode
  --disable-file-system
  --disable-gpu
  --disable-software-rasterizer
  --disable-dev-shm-usage
stopsignal=INT
autorestart=true
priority=800
user=%(ENV_USER)s
stdout_logfile=/var/log/neko/chromium.log
stdout_logfile_maxbytes=100MB
stdout_logfile_backups=10
redirect_stderr=true

[program:openbox]
command=/usr/bin/openbox --config-file /etc/neko/openbox.xml
autorestart=true
"""


class RoomCDPTests(unittest.TestCase):
    def test_only_native_profile_and_debug_flags_change(self):
        updated, _ = enable_cdp.enable(NATIVE_CHROMIUM, {"DeveloperToolsAvailability": 2})
        self.assertEqual(updated.replace(enable_cdp.CDP_PROFILE, enable_cdp.UPSTREAM_PROFILE), NATIVE_CHROMIUM)
        self.assertEqual(updated.count("command=/usr/bin/chromium\n"), 1)
        self.assertEqual(updated.count("--user-data-dir="), 1)
        self.assertIn("--user-data-dir=/home/neko/profile\n", updated)
        self.assertIn("--remote-debugging-address=127.0.0.1\n", updated)
        self.assertIn("--remote-debugging-port=9222\n", updated)
        self.assertNotIn("--headless", updated)
        self.assertNotIn("--remote-allow-origins", updated)

    def test_unrelated_managed_policy_values_survive(self):
        policies = {"DeveloperToolsAvailability": 2, "URLBlocklist": ["file://*"],
                    "ExtensionInstallBlocklist": ["*"], "PasswordManagerEnabled": False}
        _, result = enable_cdp.enable(NATIVE_CHROMIUM, policies)
        self.assertEqual(result, dict(policies, DeveloperToolsAvailability=0, RemoteDebuggingAllowed=True))
        self.assertEqual(policies["DeveloperToolsAvailability"], 2, "must not mutate caller state")
        json.dumps(result)

    def test_pinned_launcher_or_policy_drift_fails_instead_of_launching_a_second_browser(self):
        for launcher in (NATIVE_CHROMIUM.replace("/usr/bin/chromium", "/usr/bin/other"),
                         NATIVE_CHROMIUM.replace(enable_cdp.UPSTREAM_PROFILE, ""),
                         NATIVE_CHROMIUM + enable_cdp.UPSTREAM_PROFILE,
                         NATIVE_CHROMIUM + "  --remote-debugging-port=9999\n",
                         NATIVE_CHROMIUM + "  --remote-allow-origins=*\n"):
            with self.subTest(launcher=launcher), self.assertRaises(ValueError):
                enable_cdp.enable(launcher, {"DeveloperToolsAvailability": 2})
        for policy in ({}, {"DeveloperToolsAvailability": 0}):
            with self.subTest(policy=policy), self.assertRaises(ValueError):
                enable_cdp.enable(NATIVE_CHROMIUM, policy)

    def test_socat_preserves_chromium_host_and_origin_checks(self):
        config = configparser.ConfigParser(interpolation=None)
        config.read(ADAPTER / "relay.conf")
        relay = config["program:cdp-relay"]
        self.assertEqual(relay["command"],
                         "/usr/bin/socat TCP4-LISTEN:9223,bind=0.0.0.0,reuseaddr,fork TCP4:127.0.0.1:9222")
        self.assertEqual(relay["user"], "neko")
        self.assertTrue(relay.getboolean("stopasgroup"))
        self.assertTrue(relay.getboolean("killasgroup"))
        self.assertEqual(len(config.sections()), 1)

    def test_deployment_does_not_replace_neko_core_ui_or_startup(self):
        dockerfile = (ADAPTER / "Dockerfile").read_text()
        instructions = [line.strip() for line in dockerfile.splitlines()
                        if line.strip() and not line.lstrip().startswith("#")]
        self.assertIn("FROM ghcr.io/m1k1o/neko/chromium:3.2.0@sha256:270face8b8111e639744fb7bcdbbf0deed227900bb89d58b2f8adfc12af8c41e", instructions)
        self.assertFalse(any(line.split()[0] in ("CMD", "ENTRYPOINT", "EXPOSE", "HEALTHCHECK")
                             for line in instructions))
        copies = [line for line in instructions if line.startswith("COPY ")]
        self.assertEqual(copies, ["COPY enable.py /tmp/neko-enable-cdp.py",
                                  "COPY relay.conf /etc/neko/supervisord/cdp-relay.conf"])
        self.assertIn("test -x /usr/bin/socat", dockerfile)

    def test_default_tag_matches_example_settings(self):
        spec = importlib.util.spec_from_file_location("cdp_example_demo", HERE / "demo.py")
        demo = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(demo)
        configured = dict(line.split("=", 1) for line in (HERE / ".env.example").read_text().splitlines()
                          if line and not line.startswith("#"))
        self.assertEqual(configured["ROOM_IMAGE"], demo.DEFAULTS["ROOM_IMAGE"])
        self.assertEqual(demo.ROOM_IMAGE, "neko-single-port-chromium-cdp:3.2.0-1")


if __name__ == "__main__":
    unittest.main(verbosity=2)
