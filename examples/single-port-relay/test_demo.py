#!/usr/bin/env python3
"""Configuration contracts only. No image pulls, services, rooms or media tests."""
import copy
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import shutil
import socket
import stat
import subprocess
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("single_port_demo", HERE / "demo.py")
demo = importlib.util.module_from_spec(spec)
spec.loader.exec_module(demo)


def settings(**overrides):
    values = dict(demo.DEFAULTS, MANAGER_IMAGE="neko-rooms-single-port:local", ROOM_NETWORK="single-port-test",
                  ROOM_NETWORK_CIDR="172.30.82.0/24", TURN_PRIVATE_IP="172.30.82.10")
    values.update(overrides)
    return values


class TopologyTests(unittest.TestCase):
    def test_same_public_port_for_web_and_turn(self):
        for selected in ("443", "8443", "09443"):
            with self.subTest(port=selected):
                values = settings(PUBLIC_PORT=selected)
                demo.validate_topology(values, dev_ca=True, public=False)
                expected = "https://neko.localhost" + (":" + str(int(selected)) if int(selected) != 443 else "")
                self.assertEqual(values["PUBLIC_ORIGIN"], expected)
                self.assertEqual(values["PUBLIC_PORT"], str(int(selected)))

    def test_unsafe_explicit_settings_fail(self):
        invalid = [
            {"APP_HOST": "turn.localhost"}, {"APP_HOST": "127.0.0.1"}, {"APP_HOST": "https://neko.localhost"},
            {"APP_HOST": "bad..localhost"}, {"APP_HOST": "-bad.localhost"}, {"PUBLIC_BIND_IP": "0.0.0.0"},
            {"PUBLIC_PORT": "0"}, {"PUBLIC_PORT": "65536"}, {"PUBLIC_PORT": "443/udp"},
            {"TURN_RELAY_MIN": "1023"}, {"TURN_RELAY_MIN": "50000", "TURN_RELAY_MAX": "49000"},
            {"ROOM_NETWORK_CIDR": "8.8.8.0/24"}, {"ROOM_NETWORK_CIDR": "fd00::/64"},
            {"TURN_PRIVATE_IP": "172.30.82.0"}, {"TURN_PRIVATE_IP": "172.30.82.255"},
            {"TURN_PRIVATE_IP": "172.30.83.10"}, {"ROOM_NETWORK": "wrong network"},
            {"INSTANCE_NAME": "dotted.instance"},
            {"MANAGER_IMAGE": "image\nother"}, {"TLS_CERT_FILE": "/supplied/cert"},
        ]
        for changes in invalid:
            with self.subTest(keys=list(changes)):
                with self.assertRaises(ValueError):
                    demo.validate_topology(settings(**changes), dev_ca=True, public=False)

    def test_dev_ca_never_enables_public_binding(self):
        with self.assertRaises(ValueError):
            demo.validate_topology(settings(), dev_ca=True, public=True)
        with self.assertRaises(ValueError):
            demo.validate_topology(settings(), dev_ca=False, public=False)
        real = settings(APP_HOST="rooms.real-domain.org", TURN_HOST="turn.real-domain.org", PUBLIC_BIND_IP="0.0.0.0")
        with self.assertRaises(ValueError):
            demo.validate_topology(real.copy(), dev_ca=False, public=False)
        demo.validate_topology(real, dev_ca=False, public=True)

    def test_sni_preread_defaults_to_turn_not_http(self):
        nginx = (HERE / "nginx.conf.template").read_text()
        self.assertIn("map $ssl_preread_server_name $tls_backend", nginx)
        self.assertIn("@APP_HOST@ 127.0.0.1:8443;", nginx)
        self.assertIn("default 127.0.0.1:5349;", nginx)
        self.assertIn("listen 443;\n        ssl_preread on;", nginx)
        self.assertNotIn("listen 443 ssl", nginx)
        self.assertIn("listen 127.0.0.1:8443 ssl;", nginx)
        self.assertIn("listen 127.0.0.1:5349 ssl;", nginx)
        self.assertEqual(nginx.count("resolver 127.0.0.11 valid=10s ipv6=off;"), 2)
        self.assertIn("proxy_pass $turn_backend;", nginx)
        self.assertIn("set $manager http://neko-rooms:8080;", nginx)

    def test_private_turn_policy_preserved(self):
        turn = (HERE / "turnserver.conf.template").read_text().splitlines()
        for setting in ("no-udp", "no-tls", "no-dtls", "no-tcp-relay", "lt-cred-mech", "no-multicast-peers",
                        "denied-peer-ip=0.0.0.0-255.255.255.255", "allowed-peer-ip=@ROOM_PEER_RANGE@",
                        "user-quota=100", "total-quota=100"):
            self.assertIn(setting, turn)
        self.assertFalse(any(line.startswith(("external-ip", "multiplex-peer", "server-relay")) for line in turn))

    def test_network_validation_rejects_wrong_driver_subnet_reserved_and_occupied(self):
        values = settings()
        demo.validate_topology(values, dev_ca=True, public=False)
        actual = {"Driver": "bridge", "IPAM": {"Config": [{"Subnet": "172.30.82.0/24", "Gateway": "172.30.82.1"}]}, "Containers": {}}
        variants = [copy.deepcopy(actual) for _ in range(4)]
        variants[0]["Driver"] = "host"
        variants[1]["IPAM"]["Config"][0]["Subnet"] = "172.30.83.0/24"
        variants[2]["IPAM"]["Config"][0]["AuxiliaryAddresses"] = {"reserved": "172.30.82.10"}
        variants[3]["Containers"] = {"other": {"Name": "another-turn", "IPv4Address": "172.30.82.10/24"}}
        network, turn_ip = ipaddress.ip_network(values["ROOM_NETWORK_CIDR"]), ipaddress.ip_address(values["TURN_PRIVATE_IP"])
        for item in variants:
            response = subprocess.CompletedProcess([], 0, json.dumps([item]), "")
            with mock.patch.object(demo.subprocess, "run", return_value=response), self.assertRaises(ValueError):
                demo.check_network(values, network, turn_ip)
        for name in (None, values["COMPOSE_PROJECT_NAME"] + "-turn-1"):
            if name:
                actual["Containers"] = {"own": {"Name": name, "IPv4Address": "172.30.82.10/24"}}
            response = subprocess.CompletedProcess([], 0, json.dumps([actual]), "")
            with mock.patch.object(demo.subprocess, "run", return_value=response):
                demo.check_network(values, network, turn_ip)


@unittest.skipUnless(shutil.which("openssl"), "OpenSSL is required for certificate contract tests")
class ConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.umask = os.umask(0o077)
        self.temp = tempfile.TemporaryDirectory(prefix="neko-single-port-tests-$'")
        self.root = Path(self.temp.name)
        self.state = self.root / "state-$'quoted"
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.bind(str(self.root / "docker.sock"))
        self.values = settings(DOCKER_SOCKET=str(self.root / "docker.sock"),
                               OWNER_USERNAME="owner-$'\"#", OWNER_PASSWORD="\"$password#'\\value\"  ")
        self.env = self.root / "settings.env"
        self.write_settings()
        self.network_mock = mock.patch.object(demo, "check_network")
        self.network_mock.start()

    def tearDown(self):
        self.network_mock.stop()
        self.sock.close()
        self.temp.cleanup()
        os.umask(self.umask)

    def write_settings(self):
        self.env.write_text("\n".join(k + "=" + v for k, v in self.values.items()) + "\n")
        self.env.chmod(0o600)

    def configure(self):
        return demo.configure(self.env, self.state, dev_ca=True)

    def test_literal_settings_and_rejects_unknown_duplicate_or_missing(self):
        self.assertEqual(demo.read_settings(self.env)["OWNER_PASSWORD"], self.values["OWNER_PASSWORD"])
        initial = self.env.read_text()
        for addition in ("UNKNOWN=anything\n", "APP_HOST=other.localhost\n"):
            self.env.write_text(initial + addition)
            with self.assertRaises(ValueError):
                demo.read_settings(self.env)
        self.env.write_text("APP_HOST=neko.localhost\n")
        with self.assertRaises(ValueError):
            demo.read_settings(self.env)
        self.write_settings()
        self.env.chmod(0o644)
        with self.assertRaises(ValueError):
            demo.read_settings(self.env)

    def test_repeat_configure_preserves_credentials_ca_and_storage(self):
        self.configure()
        previous = {name: (self.state / name).read_bytes() for name in
                    ("credentials.json", "tls/ca.crt", "tls/ca.key", "tls/server.crt", "tls/server.key", "manager.env")}
        (self.state / "storage" / "owned-data").write_text("keep me")
        self.configure()
        self.assertTrue(all((self.state / name).read_bytes() == content for name, content in previous.items()),
                        "reconfigure must not rotate credentials or CA")
        self.assertEqual((self.state / "storage" / "owned-data").read_text(), "keep me")
        self.assertEqual(stat.S_IMODE(self.state.stat().st_mode), 0o700)
        for item in self.state.rglob("*"):
            if item.is_file():
                self.assertEqual(stat.S_IMODE(item.stat().st_mode), 0o600, item.name)
        for value in json.loads((self.state / "credentials.json").read_text()).values():
            self.assertNotIn(value, (self.state / "compose.env").read_text(), "credentials must not enter Compose interpolation")

    def test_native_room_payload_and_raw_manager_environment(self):
        self.configure()
        request = json.loads((self.state / "room-request.json").read_text())
        self.assertEqual(request["api_version"], 3)
        self.assertEqual(request["neko_image"], demo.ROOM_IMAGE)
        self.assertEqual(request["name"], "native")
        self.assertEqual([m["container_path"] for m in request["mounts"]], ["/home/neko/profile", "/home/neko/Downloads"])
        self.assertNotEqual(request["user_pass"], request["admin_pass"])
        manager = dict(line.split("=", 1) for line in (self.state / "manager.env").read_text().splitlines())
        self.assertTrue(manager["NEKO_ROOMS_ADMIN_PASSWORD"] == self.values["OWNER_PASSWORD"], "literal owner password changed")
        ice = json.loads(manager["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])
        self.assertEqual(ice[0]["urls"], ["turns:turn.localhost:8443?transport=tcp"])
        nginx = (self.state / "nginx.conf").read_text()
        self.assertIn('"neko.localhost:8443" 1;', nginx)
        self.assertIn("proxy_set_header Host $http_host;", nginx)
        self.assertIn("proxy_set_header X-Forwarded-Proto https;", nginx)
        self.assertIn("proxy_set_header X-Forwarded-Port 8443;", nginx)
        self.assertIn("proxy_set_header Authorization $http_authorization;", nginx)
        for route in ("/manage/", "/room/", "/rooms/"):
            self.assertRegex(nginx, r"location " + route + r"\s+\{ proxy_pass \$manager\$request_uri; \}")
        self.assertNotIn("aggregate.html", nginx)
        self.assertNotIn("@APP_HOST@", nginx)

    def test_generated_ice_uses_same_configured_port_including_443(self):
        for selected in ("443", "9443"):
            with self.subTest(port=selected):
                self.values["PUBLIC_PORT"] = selected
                self.write_settings()
                self.configure()
                manager = dict(line.split("=", 1) for line in (self.state / "manager.env").read_text().splitlines())
                ice = json.loads(manager["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])
                self.assertEqual(ice[0]["urls"], [f"turns:turn.localhost:{selected}?transport=tcp"])
                authority = "neko.localhost" + (":" + selected if selected != "443" else "")
                self.assertIn("PUBLIC_ORIGIN='https://" + authority + "'", (self.state / "compose.env").read_text())
                self.assertIn("proxy_set_header X-Forwarded-Port " + selected + ";", (self.state / "nginx.conf").read_text())

    def test_no_implicit_credential_rotation_or_partial_pairs(self):
        self.configure()
        previous = (self.state / "credentials.json").read_bytes()
        self.values["OWNER_PASSWORD"] = "changed-password"
        self.write_settings()
        with self.assertRaisesRegex(ValueError, "no implicit credential rotation"):
            self.configure()
        self.assertEqual((self.state / "credentials.json").read_bytes(), previous)
        self.values["OWNER_PASSWORD"] = ""
        self.write_settings()
        with self.assertRaisesRegex(ValueError, "set both OWNER"):
            self.configure()

    def test_ca_covers_both_names_and_rejects_san_change(self):
        self.configure()
        cert = str(self.state / "tls/server.crt")
        for host in ("neko.localhost", "turn.localhost"):
            demo.run_openssl("verify", "-CAfile", str(self.state / "tls/ca.crt"), "-verify_hostname", host, cert)
        previous = (self.state / "tls/server.crt").read_bytes()
        self.values["TURN_HOST"] = "changed.localhost"
        self.write_settings()
        with self.assertRaises(ValueError):
            self.configure()
        self.assertEqual((self.state / "tls/server.crt").read_bytes(), previous)

    def test_checkout_and_unowned_state_are_refused(self):
        with self.assertRaisesRegex(ValueError, "outside the source checkout"):
            demo.configure(self.env, demo.ROOT / "must-not-create-single-port-test", dev_ca=True)
        self.state.mkdir()
        (self.state / "existing-user-data").write_text("do not touch")
        with self.assertRaisesRegex(ValueError, "not owned by this example"):
            self.configure()
        self.assertEqual(list(p.name for p in self.state.iterdir()), ["existing-user-data"])

    def test_state_symlink_and_invalid_marker_fail_before_modifying_directory(self):
        actual = self.root / "unowned"
        actual.mkdir(mode=0o755)
        self.state.symlink_to(actual, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlink state"):
            self.configure()
        self.assertFalse(any(actual.iterdir()))
        self.state.unlink()
        self.state.mkdir(mode=0o755)
        (self.state / ".single-port-relay").write_text("other owner")
        previous_mode = stat.S_IMODE(self.state.stat().st_mode)
        with self.assertRaisesRegex(ValueError, "ownership marker does not match"):
            self.configure()
        self.assertEqual(stat.S_IMODE(self.state.stat().st_mode), previous_mode)

    def test_compose_command_always_uses_explicit_private_environment(self):
        self.configure()
        command = demo.compose_command(self.state, ["ps"])
        self.assertEqual(command[:3], ["docker", "compose", "--env-file"])
        self.assertEqual(command[3], str(self.state / "compose.env"))
        self.assertEqual(command[-3:], ["-f", str(HERE / "compose.yaml"), "ps"])

    def test_actual_compose_parse_one_socket_native_mode_and_literal_secrets(self):
        if not shutil.which("docker") or subprocess.run(["docker", "compose", "version"], capture_output=True).returncode:
            self.skipTest("Docker Compose unavailable; source-only contracts still run")
        self.configure()
        environment = {k: v for k, v in os.environ.items() if k not in demo.COMPOSE_KEYS}
        result = subprocess.run(demo.compose_command(self.state, ["config", "--format", "json"]),
                                env=environment, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, "Compose config rejected private generated input (output withheld)")
        # `compose config` doubles $ for round-trip serialization, even as JSON;
        # undo that serialization step before comparing literal container values.
        project = json.loads(result.stdout.replace("$$", "$"))
        services = project["services"]
        ports = services["gateway"]["ports"]
        self.assertEqual(len(ports), 1)
        self.assertEqual((ports[0]["host_ip"], ports[0]["published"], ports[0]["target"], ports[0]["protocol"]),
                         ("127.0.0.1", "8443", 443, "tcp"))
        for name in ("turn", "neko-rooms"):
            self.assertFalse(services[name].get("ports"))
            self.assertNotEqual(services[name].get("network_mode"), "host")
        self.assertEqual(services["neko-rooms"]["pull_policy"], "never")
        env = services["neko-rooms"]["environment"]
        self.assertEqual(env["NEKO_ROOMS_NETWORK_MODE"], "relay")
        self.assertEqual(env["NEKO_ROOMS_NEKO_IMAGES"], demo.ROOM_IMAGE)
        self.assertEqual(env["NEKO_ROOMS_PATH_PREFIX"], "/room/")
        self.assertEqual(env["NEKO_ROOMS_INSTANCE_URL"], "https://neko.localhost:8443/")
        self.assertEqual(env["NEKO_ROOMS_AGENT_PUBLIC_URL"], "https://neko.localhost:8443/rooms/")
        self.assertTrue(env["NEKO_ROOMS_ADMIN_PASSWORD"] == self.values["OWNER_PASSWORD"], "raw env changed secret characters")
        self.assertTrue(env["NEKO_ROOMS_ADMIN_USERNAME"] == self.values["OWNER_USERNAME"], "raw env changed username characters")
        self.assertEqual(json.loads(env["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])[0]["urls"],
                         ["turns:turn.localhost:8443?transport=tcp"])
        gateway_mount = next(v for v in services["gateway"]["volumes"] if v["target"] == "/etc/nginx/nginx.conf")
        self.assertEqual(gateway_mount["source"], str(self.state / "nginx.conf"))
        self.assertEqual(env["NEKO_ROOMS_STORAGE_EXTERNAL"], str(self.state / "storage"))
        self.assertTrue(project["networks"]["rooms"]["external"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
