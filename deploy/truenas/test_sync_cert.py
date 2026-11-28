#!/usr/bin/env python3
"""Unit tests for sync-cert.py (no real Docker or TrueNAS required)."""
import hashlib
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("tn_sync_cert", HERE / "sync-cert.py")
sc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sc)


class SyncCertTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="tn-sync-cert-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.cert = self.base / "cert.crt"
        self.key = self.base / "cert.key"
        self.cert.write_bytes(b"new-cert-pem")
        self.key.write_bytes(b"new-key-pem")
        self.key.chmod(0o600)
        self.cert_sha = hashlib.sha256(b"new-cert-pem").hexdigest()
        self.key_sha = hashlib.sha256(b"new-key-pem").hexdigest()
        self.settings = self.base / "native-relay.settings"
        self.write_settings(TRANSPORT="tls", TLS_CERT_FILE=str(self.cert), TLS_KEY_FILE=str(self.key))

    def write_settings(self, **overrides):
        vals = {
            "TRANSPORT": "tls",
            "APP_HOST": "neko.example.test",
            "TURN_HOST": "neko.example.test",
            "PUBLIC_PORT": "22222",
            "EXTERNAL_PORT": "443",
            "PUBLIC_BIND_IP": "192.168.202.11",
            "ROOM_NETWORK_CIDR": "172.16.14.0/24",
            "TLS_CERT_FILE": "",
            "TLS_KEY_FILE": "",
        }
        vals.update(overrides)
        self.settings.write_text("\n".join(f"{k}={v}" for k, v in vals.items()) + "\n")
        self.settings.chmod(0o600)

    def test_plain_or_dev_ca_mode_is_skipped(self):
        self.write_settings(TRANSPORT="plain")
        self.assertEqual(sc.sync_certificate(self.settings)["status"], "skipped")
        self.write_settings(TRANSPORT="tls", TLS_CERT_FILE="", TLS_KEY_FILE="")
        self.assertEqual(sc.sync_certificate(self.settings)["status"], "skipped")

    def test_half_configured_tls_fails_closed(self):
        self.write_settings(TRANSPORT="tls", TLS_CERT_FILE=str(self.cert), TLS_KEY_FILE="")
        with self.assertRaisesRegex(ValueError, "must both be set"):
            sc.sync_certificate(self.settings)

    def test_unchanged_cert_is_noop(self):
        with patch.object(sc.cfg, "check_certificate"), \
             patch.object(sc, "pem_leaf_sha256", return_value="aaa111"), \
             patch.object(sc, "probe_live_leaf_sha256", return_value="aaa111"), \
             patch.object(sc.cfg, "docker", side_effect=AssertionError("docker must not be called")):
            res = sc.sync_certificate(self.settings)
        self.assertEqual(res, {"status": "unchanged", "fingerprint": "aaa111"})

    def test_same_inode_renewal_runs_nginx_test_and_graceful_reload(self):
        calls = []

        def fake_docker(*args):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, "", "")

        def fake_mounted(container, path):
            return self.cert_sha if path.endswith(".crt") else self.key_sha

        with patch.object(sc.cfg, "check_certificate"), \
             patch.object(sc, "pem_leaf_sha256", return_value="new222"), \
             patch.object(sc, "container_file_sha256", side_effect=fake_mounted), \
             patch.object(sc, "probe_live_leaf_sha256", side_effect=["old111", "new222"]), \
             patch.object(sc.cfg, "docker", side_effect=fake_docker):
            res = sc.sync_certificate(self.settings)
        self.assertEqual(res["status"], "reloaded")
        self.assertEqual(res["action"], "nginx_reload")
        self.assertEqual(calls, [
            ("exec", "ix-neko-gateway-1", "nginx", "-t"),
            ("exec", "ix-neko-gateway-1", "nginx", "-s", "reload"),
        ])

    def test_replaced_cert_or_key_inode_restarts_only_gateway(self):
        for stale_path in ("/run/relay/tls.crt", "/run/relay/tls.key"):
            calls = []

            def fake_docker(*args):
                calls.append(args)
                return subprocess.CompletedProcess(args, 0, "", "")

            def fake_mounted(container, path, stale=stale_path):
                if path == stale:
                    return "stale-inode-hash"
                return self.cert_sha if path.endswith(".crt") else self.key_sha

            with self.subTest(stale=stale_path), \
                 patch.object(sc.cfg, "check_certificate"), \
                 patch.object(sc, "pem_leaf_sha256", return_value="new222"), \
                 patch.object(sc, "container_file_sha256", side_effect=fake_mounted), \
                 patch.object(sc, "probe_live_leaf_sha256", side_effect=["old111", "new222"]), \
                 patch.object(sc.cfg, "docker", side_effect=fake_docker):
                res = sc.sync_certificate(self.settings)
            self.assertEqual(res["status"], "reloaded")
            self.assertEqual(res["action"], "gateway_restart")
            self.assertEqual(calls, [("restart", "ix-neko-gateway-1")])

    def test_invalid_host_cert_fails_closed_before_touching_gateway(self):
        with patch.object(sc.cfg, "check_certificate", side_effect=ValueError("OpenSSL validation failed")), \
             patch.object(sc.cfg, "docker", side_effect=AssertionError("docker must not be called")):
            with self.assertRaisesRegex(ValueError, "OpenSSL"):
                sc.sync_certificate(self.settings)


if __name__ == "__main__":
    unittest.main()
