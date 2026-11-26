#!/usr/bin/env python3
"""Temporary fixtures only: no NAS, image pulls, containers or existing Docker state."""
import http.server
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("tn_configure", HERE / "configure.py")
cfg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cfg)
RUN = subprocess.run


def compose_run(command, **kwargs):
    # Test the exact NAS parser without replacing the installed host plugin.
    binary = os.environ.get("TEST_COMPOSE_BINARY")
    if binary and command[:2] == ["docker", "compose"]:
        command = [binary, *command[2:]]
    return RUN(command, **kwargs)


class ConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="tn-native-test-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.compose_template = self.base / "neko.compose.yaml"
        manifest = Path(os.environ.get("TEST_COMPOSE_TEMPLATE", cfg.COMPOSE_TEMPLATE))
        self.compose_template.write_bytes(manifest.read_bytes())
        self.app = self.base / "app # dollar$ and 'quote"
        self.app.mkdir()
        self.settings = self.base / "settings"
        self.values = dict(cfg.DEFAULTS, APP_HOST="neko.private.test", TURN_HOST="neko.private.test",
                           PUBLIC_BIND_IP="127.0.0.1", ROOM_NETWORK_CIDR="172.29.70.0/24", TURN_PRIVATE_IP="172.29.70.10")
        for name in ("profile", "downloads", "rooms-data", "config"):
            (self.app / name).mkdir()
        self.files = {
            "rooms.env": "NEKO_ROOMS_ADMIN_USERNAME=existing-owner\nNEKO_ROOMS_ADMIN_PASSWORD='keep-owner-$literal#quote'\n",
            "neko.env": "NEKO_MEMBER_MULTIUSER_USER_PASSWORD=keep-viewer\nNEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD=keep-viewer-admin\n",
            "cdp.htpasswd": "fixture-only-hash\n", "config/chromium.conf": "keep launcher\n",
            "config/chromium-policies.json": '{"keep":"policy"}\n',
            "profile/existing": "keep default data\n", "downloads/existing": "keep download\n",
            "rooms-data/old-acceptance": "keep all old room data\n",
        }
        for name, content in self.files.items():
            (self.app / name).write_text(content)
            (self.app / name).chmod(0o600)
        previous_umask = os.umask(0o077)
        self.addCleanup(os.umask, previous_umask)
        runner = patch.object(cfg.subprocess, "run", side_effect=compose_run)
        runner.start()
        self.addCleanup(runner.stop)
        network = patch.object(cfg, "check_network")
        network.start()
        self.addCleanup(network.stop)
        self.write_settings()

    def write_settings(self):
        self.settings.write_text("\n".join(k + "=" + v for k, v in self.values.items()) + "\n")
        self.settings.chmod(0o600)

    def configure(self, **kwargs):
        return cfg.configure(self.settings, self.app, dev_ca=True, compose_template=self.compose_template, **kwargs)[0]

    def model(self, path):
        result = compose_run(["docker", "compose", "--project-name", "ix-neko", "--env-file", "/dev/null",
                              "-f", str(path), "config", "--format", "json"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        # Compose config escapes every dollar for re-ingestion, even in JSON.
        return json.loads(result.stdout.replace("$$", "$"))

    def test_native_root_render_preserves_data_secrets_and_ports(self):
        state = self.configure()
        yaml = (state / "neko.compose.yaml").read_text()
        self.assertNotIn("${", yaml)
        for password in ("keep-owner", "keep-viewer", "keep-viewer-admin"):
            self.assertNotIn(password, yaml)
        model = self.model(state / "neko.compose.yaml")
        services = model["services"]
        manager = services["neko-rooms"]
        self.assertEqual(manager["image"], cfg.DEFAULTS["MANAGER_IMAGE"])
        self.assertEqual(manager["pull_policy"], "always")
        self.assertEqual(manager["environment"]["NEKO_ROOMS_NETWORK_MODE"], "relay")
        self.assertEqual(manager["environment"]["NEKO_ROOMS_NEKO_IMAGES"], services["room-image"]["image"])
        self.assertEqual(manager["environment"]["NEKO_ROOMS_ADMIN_PASSWORD"], "keep-owner-$literal#quote")
        self.assertEqual(manager["environment"]["NEKO_ROOMS_STORAGE_EXTERNAL"], str(self.app / "rooms-data"))
        self.assertEqual(manager["environment"]["NEKO_ROOMS_INSTANCE_NAME"], "tn-neko-rooms")
        self.assertEqual(manager["depends_on"]["room-image"]["condition"], "service_completed_successfully")
        self.assertEqual(services["room-image"]["network_mode"], "none")
        self.assertEqual(services["room-image"]["entrypoint"], ["/bin/true"])
        self.assertFalse(services["room-image"].get("volumes"))
        for name in ("neko-rooms", "turn", "classifier", "room-image"):
            self.assertFalse(services[name].get("ports"), name)
        published = {(p["published"], p["protocol"]) for s in services.values() for p in s.get("ports", [])}
        expected = {("28320", "tcp"), ("28321", "tcp"), ("28322", "tcp")}
        expected.update((str(p), "udp") for p in range(52100, 52111))
        self.assertEqual(published, expected)
        self.assertEqual(services["gateway"]["ports"][0]["host_ip"], "127.0.0.1")
        self.assertEqual(model["networks"]["rooms"]["name"], "neko-rooms-net")
        self.assertTrue(model["networks"]["rooms"]["external"])
        for name in ("gateway", "classifier", "turn"):
            self.assertIn("@sha256:", services[name]["image"])
        for name, content in self.files.items():
            self.assertEqual((self.app / name).read_text(), content, name)
        mounts = {v["target"]: v["source"] for v in services["neko"]["volumes"]}
        self.assertEqual(mounts["/home/neko/profile"], str(self.app / "profile"))
        self.assertEqual(mounts["/home/neko/Downloads"], str(self.app / "downloads"))
        ice = json.loads(manager["environment"]["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])
        saved = json.loads((state / "turn-credentials.json").read_text())
        self.assertEqual(ice[0]["urls"], ["turns:neko.private.test:28322?transport=tcp"])
        self.assertEqual(ice[0]["credential"], saved["password"])
        self.assertNotIn(saved["password"], yaml)
        self.assertEqual(services["turn"]["networks"]["rooms"]["ipv4_address"], "172.29.70.10")

    def test_ipv4_hosts_generate_ip_san_and_keep_existing_auth(self):
        self.values.update(APP_HOST="192.168.202.11", TURN_HOST="")
        self.write_settings()
        state = self.configure()
        services = self.model(state / "neko.compose.yaml")["services"]
        self.assertEqual(services["neko-rooms"]["environment"]["NEKO_ROOMS_INSTANCE_URL"], "https://192.168.202.11:28322/")
        ice = json.loads((state / "relay.env").read_text().split("=", 1)[1])
        self.assertEqual(ice[0]["urls"], ["turns:192.168.202.11:28322?transport=tcp"])
        result = RUN(["openssl", "x509", "-in", str(state / "tls/server.crt"), "-noout", "-ext", "subjectAltName"],
                     capture_output=True, text=True, check=True)
        self.assertIn("IP Address:192.168.202.11", result.stdout)
        self.assertNotIn("DNS:192.168.202.11", result.stdout)
        with self.assertRaisesRegex(ValueError, "OpenSSL"):
            cfg.check_certificate(state / "tls/server.crt", state / "tls/server.key",
                                  dict(self.values, TURN_HOST="192.168.202.12"))
        for name, content in self.files.items():
            self.assertEqual((self.app / name).read_text(), content, name)

    def test_mixed_dns_ip_certificate_covers_both_host_types(self):
        self.values.update(APP_HOST="192.168.202.11", TURN_HOST="turn.private.test")
        self.write_settings()
        state = self.configure()
        cfg.check_certificate(state / "tls/server.crt", state / "tls/server.key", self.values)
        san = RUN(["openssl", "x509", "-in", str(state / "tls/server.crt"), "-noout", "-ext", "subjectAltName"],
                  capture_output=True, text=True, check=True).stdout
        self.assertIn("IP Address:192.168.202.11", san)
        self.assertIn("DNS:turn.private.test", san)

    def test_ipv6_hosts_are_canonical_and_bracketed_in_urls(self):
        self.values.update(APP_HOST="2001:0db8::11", TURN_HOST="")
        self.write_settings()
        state = self.configure()
        services = self.model(state / "neko.compose.yaml")["services"]
        self.assertEqual(services["neko-rooms"]["environment"]["NEKO_ROOMS_INSTANCE_URL"], "https://[2001:db8::11]:28322/")
        ice = json.loads((state / "relay.env").read_text().split("=", 1)[1])
        self.assertEqual(ice[0]["urls"], ["turns:[2001:db8::11]:28322?transport=tcp"])
        cfg.check_certificate(state / "tls/server.crt", state / "tls/server.key",
                              dict(self.values, APP_HOST="2001:db8::11", TURN_HOST="2001:db8::11"))

    def test_auto_turn_uses_docker_dns_without_static_endpoint_or_relay_ip(self):
        self.values["TURN_PRIVATE_IP"] = "auto"
        self.write_settings()
        state = self.configure()
        turn = self.model(state / "neko.compose.yaml")["services"]["turn"]
        self.assertFalse(turn["networks"]["rooms"].get("ipv4_address"))
        text = (state / "turnserver.conf").read_text()
        self.assertIn("listening-ip=0.0.0.0", text)
        self.assertNotIn("relay-ip=", text)
        self.assertIn("turn:3478", (state / "haproxy.cfg").read_text())
        for name, content in self.files.items():
            self.assertEqual((self.app / name).read_text(), content)

    def test_idempotent_setup_keeps_permanent_ca_keys_and_turn(self):
        state = self.configure()
        names = ("tls/ca.crt", "tls/ca.key", "tls/server.crt", "tls/server.key", "turn-credentials.json")
        before = {n: (state / n).read_bytes() for n in names}
        self.configure()
        self.assertEqual(before, {n: (state / n).read_bytes() for n in names})
        for name in ("relay.env", "turnserver.conf", "nginx.conf", "neko.compose.yaml", "tls/server.key"):
            self.assertEqual((state / name).stat().st_mode & 0o777, 0o600, name)
        self.assertEqual((state / "haproxy.cfg").stat().st_mode & 0o777, 0o644)
        self.assertEqual((state / "public/ca.crt").stat().st_mode & 0o777, 0o644)
        self.assertEqual((state / "public/ca.crt").read_bytes(), (state / "tls/ca.crt").read_bytes())

    def test_renew_leaf_is_explicit_and_never_rotates_ca_keys(self):
        state = self.configure()
        names = ("tls/ca.crt", "tls/ca.key", "tls/server.key", "turn-credentials.json")
        saved = {n: (state / n).read_bytes() for n in names}
        old_leaf = (state / "tls/server.crt").read_bytes()
        self.values.update(APP_HOST="new.neko.private.test", TURN_HOST="new.neko.private.test")
        self.write_settings()
        with self.assertRaisesRegex(ValueError, "OpenSSL"):
            self.configure()
        self.assertEqual(old_leaf, (state / "tls/server.crt").read_bytes())
        self.configure(renew=True)
        self.assertNotEqual(old_leaf, (state / "tls/server.crt").read_bytes())
        self.assertEqual(saved, {n: (state / n).read_bytes() for n in names})

    def test_existing_certificates_are_used_without_modification(self):
        state = self.configure()
        self.values.update(TLS_CERT_FILE=str(state / "tls/server.crt"), TLS_KEY_FILE=str(state / "tls/server.key"))
        self.write_settings()
        before = {p.name: p.read_bytes() for p in (state / "tls").iterdir() if p.is_file()}
        cfg.configure(self.settings, self.app, compose_template=self.compose_template)
        self.assertEqual(before, {p.name: p.read_bytes() for p in (state / "tls").iterdir() if p.is_file()})
        self.assertNotIn("location = /ca.crt", (state / "nginx.conf").read_text())

    def test_pin_and_offline_override_are_explicit_and_consistent(self):
        self.values.update(MANAGER_IMAGE="docker.io/ylangylang/neko-rooms:single-port-97ffae9",
                           ROOM_IMAGE="docker.io/ylangylang/neko-chromium-cdp:3.2.0-1", RELEASE_PULL_POLICY="never")
        self.write_settings()
        state = self.configure()
        services = self.model(state / "neko.compose.yaml")["services"]
        self.assertEqual(services["neko-rooms"]["image"], self.values["MANAGER_IMAGE"])
        self.assertEqual(services["room-image"]["image"], self.values["ROOM_IMAGE"])
        self.assertEqual(services["neko-rooms"]["environment"]["NEKO_ROOMS_NEKO_IMAGES"], self.values["ROOM_IMAGE"])
        for name in ("neko-rooms", "room-image"):
            self.assertEqual(services[name]["pull_policy"], "never")

    def test_supplied_environment_cannot_override_rendered_choices(self):
        with patch.dict(os.environ, {"ROOM_IMAGE": "unwanted:latest", "PUBLIC_PORT": "9999"}):
            state = self.configure()
        services = self.model(state / "neko.compose.yaml")["services"]
        self.assertEqual(services["room-image"]["image"], cfg.DEFAULTS["ROOM_IMAGE"])
        self.assertEqual(services["gateway"]["ports"][0]["published"], "28322")

    def test_blank_turn_host_uses_same_explicit_dns_name(self):
        self.values["TURN_HOST"] = ""
        self.write_settings()
        state = self.configure()
        self.assertIn("turns:neko.private.test:28322", (state / "relay.env").read_text())
        self.assertNotIn("neko.private.test 127.0.0.1:8443;", (state / "nginx.conf").read_text())

    def test_native_transport_templates_and_single_port_headers(self):
        state = self.configure()
        nginx = (state / "nginx.conf").read_text()
        classifier = (state / "haproxy.cfg").read_text()
        turn = (state / "turnserver.conf").read_text()
        self.assertIn("default 127.0.0.1:5349;", nginx)
        self.assertIn("proxy_set_header X-Forwarded-Proto https;", nginx)
        self.assertIn("proxy_set_header X-Forwarded-Port 28322;", nginx)
        self.assertIn("location /room/", nginx)
        for text in ("req.proto_http", "2112a442", "reject if WAIT_END", "init-addr none"):
            self.assertIn(text, classifier)
        for text in ("listening-ip=172.29.70.10", "no-tcp-relay", "allowed-peer-ip=172.29.70.1-172.29.70.254"):
            self.assertIn(text, turn)
        self.assertNotRegex(nginx + classifier + turn, r"@[A-Z_]+@")

    def test_postdeploy_probe_verifies_tls_without_assuming_dns_is_bind_ip(self):
        state = self.configure()

        class OwnerForm(http.server.BaseHTTPRequestHandler):
            challenge = False

            def do_GET(self):
                self.send_response(401 if self.challenge else 200)
                if self.challenge:
                    self.send_header("WWW-Authenticate", 'Basic realm="wrong"')
                self.end_headers()
                self.wfile.write(b"Owner form")

            def log_message(self, *args):
                pass

            def handle(self):
                try:
                    super().handle()
                except (BrokenPipeError, ConnectionResetError):
                    # TLS-only TURN probes intentionally send no HTTP request.
                    pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), OwnerForm)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(state / "tls/server.crt", state / "tls/server.key")
        server.socket = context.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.values.update(PUBLIC_BIND_IP="192.168.202.11", PUBLIC_PORT=str(server.server_port))
        self.write_settings()
        actual_resolver = socket.getaddrinfo

        def resolver(host, port, *args, **kwargs):
            self.assertEqual(host, "neko.private.test")
            return actual_resolver("127.0.0.1", port, *args, **kwargs)

        before = {p.relative_to(state): p.read_bytes() for p in state.rglob("*") if p.is_file()}
        with patch.object(cfg.socket, "getaddrinfo", side_effect=resolver):
            notes = cfg.check_endpoint(self.settings, self.app, private_ca=True)
            self.assertIn("DNS=127.0.0.1 TCP/TLS verified", notes[0])
            with self.assertRaises(ssl.SSLCertVerificationError):
                cfg.check_endpoint(self.settings, self.app)
            OwnerForm.challenge = True
            with self.assertRaisesRegex(ValueError, "Owner form endpoint"):
                cfg.check_endpoint(self.settings, self.app, private_ca=True)
        self.assertEqual(before, {p.relative_to(state): p.read_bytes() for p in state.rglob("*") if p.is_file()})

    def test_missing_saved_turn_credentials_refuses_rotation(self):
        state = self.configure()
        (state / "turn-credentials.json").unlink()
        with self.assertRaisesRegex(ValueError, "refusing implicit rotation"):
            self.configure()
        self.assertFalse((state / "turn-credentials.json").exists())

    def test_unowned_state_and_symlinks_are_not_overwritten(self):
        state = self.app / "native-relay"
        state.mkdir()
        (state / "ca.key").write_text("existing unknown key")
        with self.assertRaisesRegex(ValueError, "unowned"):
            self.configure()
        self.assertEqual((state / "ca.key").read_text(), "existing unknown key")
        (state / "ca.key").unlink()
        (state / "nginx.conf").symlink_to(self.app / "neko.env")
        with self.assertRaisesRegex(ValueError, "unowned"):
            self.configure()
        self.assertEqual((self.app / "neko.env").read_text(), self.files["neko.env"])

    def test_missing_data_and_permanent_auth_are_not_replaced(self):
        (self.app / "rooms.env").unlink()
        with self.assertRaisesRegex(ValueError, "permanent authentication"):
            self.configure()
        self.assertFalse((self.app / "rooms.env").exists())
        self.assertFalse((self.app / "native-relay").exists())

    def test_settings_reject_secret_overrides_unknown_and_duplicate_keys(self):
        for extra in ("OWNER_PASSWORD=replace\n", "PUBLIC_PORT=42\n", "TYPO=yes\n"):
            self.write_settings()
            with self.settings.open("a") as stream:
                stream.write(extra)
            with self.assertRaisesRegex(ValueError, "unknown, duplicate"):
                cfg.read_settings(self.settings)

    def test_topology_requires_valid_hosts_private_subnet_and_opt_in_bind(self):
        cases = (({"APP_HOST": "https://192.168.202.11"}, "DNS names or IP"),
                 ({"TURN_HOST": "192.168.202.11:28322"}, "DNS names or IP"),
                 ({"PUBLIC_BIND_IP": "0.0.0.0"}, "--public"),
                 ({"ROOM_NETWORK_CIDR": "203.0.113.0/24"}, "RFC1918"),
                 ({"TURN_PRIVATE_IP": "172.29.70.255"}, "usable address"),
                 ({"RELEASE_PULL_POLICY": "quiet-fallback"}, "RELEASE_PULL_POLICY"))
        for changes, message in cases:
            with self.subTest(changes=changes):
                with self.assertRaisesRegex(ValueError, message):
                    cfg.topology(dict(self.values, **changes), True, False)


class RepositoryPathTests(unittest.TestCase):
    def test_tooling_and_publisher_belong_to_source_checkout(self):
        self.assertEqual(HERE, cfg.ROOT / "deploy" / "truenas")
        self.assertTrue((cfg.ROOT / "scripts" / "release.py").is_file())
        self.assertEqual(cfg.COMPOSE_TEMPLATE, cfg.ROOT.parent / "truenas-apps" / "neko.compose.yaml")
        self.assertFalse((cfg.ROOT / "neko.compose.yaml").exists())

    def test_explicit_manifest_is_read_instead_of_a_source_repo_copy(self):
        with tempfile.TemporaryDirectory(prefix="tn-manifest-path-") as temporary:
            state = Path(temporary)
            template = state / "external.compose.yaml"
            template.write_text('services:\n  room-image:\n    image: ${ROOM_IMAGE:-unused}\n')
            result = subprocess.CompletedProcess([], 0, "", "")
            with patch.object(cfg.subprocess, "run", return_value=result):
                cfg.render_compose(state, {"ROOM_IMAGE": "example/room:pinned"}, template)
            self.assertIn('image: "example/room:pinned"', (state / "neko.compose.yaml").read_text())


class NetworkOwnershipTests(unittest.TestCase):
    def test_network_gateway_reserved_and_other_owner_rejected(self):
        network = ipaddress.ip_network("172.29.70.0/24")
        actual = {"Driver": "bridge", "IPAM": {"Config": [{"Subnet": str(network), "Gateway": "172.29.70.1"}]},
                  "Containers": {"abc": {"IPv4Address": "172.29.70.10/24"}}}
        result = subprocess.CompletedProcess([], 0, json.dumps([actual]), "")
        labels = {"com.docker.compose.project": "another-app", "com.docker.compose.service": "turn"}
        inspect = subprocess.CompletedProcess([], 0, json.dumps([{"Config": {"Labels": labels}}]), "")
        with patch.object(cfg, "docker", return_value=result):
            with self.assertRaisesRegex(ValueError, "gateway"):
                cfg.check_network(network, ipaddress.ip_address("172.29.70.1"))
        with patch.object(cfg, "docker", side_effect=[result, inspect]):
            with self.assertRaisesRegex(ValueError, "different container"):
                cfg.check_network(network, ipaddress.ip_address("172.29.70.10"))
        labels["com.docker.compose.project"] = "ix-neko"
        inspect.stdout = json.dumps([{"Config": {"Labels": labels}}])
        with patch.object(cfg, "docker", side_effect=[result, inspect]):
            cfg.check_network(network, ipaddress.ip_address("172.29.70.10"))


if __name__ == "__main__":
    unittest.main()
