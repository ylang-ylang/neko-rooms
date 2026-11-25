#!/usr/bin/env python3
"""Configuration contracts only. No image pulls, services, rooms or media tests."""
import copy
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
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
LAN_TURN_HOST = "debian-vm-2.lan"  # Syntax/SAN tests never query or modify DNS.


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
                self.assertEqual(values["HTTP_AUTHORITY"], "neko.localhost:" + str(int(selected)))

    def test_unsafe_explicit_settings_fail(self):
        invalid = [
            {"APP_HOST": "127.0.0.1"}, {"APP_HOST": "https://neko.localhost"},
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

    def test_dev_ca_requires_explicit_lan_mode_for_public_binding(self):
        with self.assertRaises(ValueError):
            demo.validate_topology(settings(), dev_ca=True, public=True)
        with self.assertRaises(ValueError):
            demo.validate_topology(settings(), dev_ca=False, public=False)
        real = settings(APP_HOST="rooms.real-domain.org", TURN_HOST="turn.real-domain.org", PUBLIC_BIND_IP="0.0.0.0")
        with self.assertRaises(ValueError):
            demo.validate_topology(real.copy(), dev_ca=False, public=False)
        demo.validate_topology(real, dev_ca=False, public=True)

    def test_explicit_lan_uses_same_ip_and_port_without_web_sni(self):
        for address in ("10.2.3.4", "172.16.1.2", "192.168.201.237"):
            for bind in ("0.0.0.0", address):
                with self.subTest(address=address, bind=bind):
                    values = settings(PUBLIC_BIND_IP=bind)
                    demo.validate_topology(values, dev_ca=True, public=True, lan_ip=address)
                    self.assertEqual(values["APP_HOST"], address)
                    self.assertEqual(values["TURN_HOST"], address)
                    self.assertEqual(values["PUBLIC_ORIGIN"], f"https://{address}:8443")
                    self.assertEqual(values["WEB_SNI_ROUTE"], "", "shared IP must not route TURN SNI to HTTPS")
                    self.assertIn("location = /ca.crt", values["DEV_CA_LOCATION"])

    def test_lan_turn_fqdn_preserves_ip_web_authority(self):
        for host in (LAN_TURN_HOST, "TURN.LAN.INTERNAL"):
            with self.subTest(host=host):
                values = settings(PUBLIC_BIND_IP="0.0.0.0", TURN_HOST="old.localhost")
                demo.validate_topology(values, True, True, "192.168.201.237", host)
                self.assertEqual(values["APP_HOST"], "192.168.201.237")
                self.assertEqual(values["TURN_HOST"], host.lower())
                self.assertEqual(values["PUBLIC_ORIGIN"], "https://192.168.201.237:8443")
                self.assertEqual(values["HTTP_AUTHORITY"], "192.168.201.237:8443")
                self.assertIn("location = /ca.crt", values["DEV_CA_LOCATION"])
        # Neither a stale settings hostname nor an old localhost identity is
        # silently carried into LAN mode when no explicit override is given.
        values = settings(PUBLIC_BIND_IP="0.0.0.0", TURN_HOST="old.turn.internal")
        demo.validate_topology(values, True, True, "192.168.201.237")
        self.assertEqual(values["TURN_HOST"], "192.168.201.237")

    def test_turn_host_override_requires_lan_and_non_localhost_dns_identity(self):
        for values, dev_ca, public in (
                (settings(), True, False),
                (settings(APP_HOST="rooms.real-domain.org", TURN_HOST="turn.real-domain.org"), False, False)):
            with self.subTest(dev_ca=dev_ca), self.assertRaisesRegex(ValueError, "--turn-host requires"):
                demo.validate_topology(values, dev_ca, public, turn_host=LAN_TURN_HOST)
        for host in ("", "192.168.201.237", "::1", "localhost", "turn.localhost", "TURN.LOCALHOST",
                     "turn", "https://turn.lan.internal", "turn.lan.internal:8443", "bad..internal", "-bad.lan.internal"):
            with self.subTest(host=host), self.assertRaises(ValueError):
                demo.validate_topology(settings(PUBLIC_BIND_IP="0.0.0.0"), True, True, "192.168.201.237", host)
        for dev_ca, public in ((False, True), (True, False)):
            with self.subTest(dev_ca=dev_ca, public=public), self.assertRaises(ValueError):
                demo.validate_topology(settings(PUBLIC_BIND_IP="0.0.0.0"), dev_ca, public, "192.168.201.237", LAN_TURN_HOST)

    def test_lan_requires_explicit_flags_rfc1918_and_matching_bind(self):
        for dev_ca, public in ((False, False), (False, True), (True, False)):
            with self.subTest(dev_ca=dev_ca, public=public), self.assertRaises(ValueError):
                demo.validate_topology(settings(PUBLIC_BIND_IP="0.0.0.0"), dev_ca, public, "192.168.201.237")
        for address in ("8.8.8.8", "127.0.0.1", "100.64.0.1", "169.254.1.1", "198.18.0.1", "192.0.2.1", "::1", "fd00::1"):
            with self.subTest(address=address), self.assertRaises(ValueError):
                demo.validate_topology(settings(PUBLIC_BIND_IP="0.0.0.0"), True, True, address)
        for bind in ("127.0.0.1", "192.168.201.238"):
            with self.subTest(bind=bind), self.assertRaises(ValueError):
                demo.validate_topology(settings(PUBLIC_BIND_IP=bind), True, True, "192.168.201.237")
        with self.assertRaises(ValueError):
            demo.validate_topology(settings(PUBLIC_BIND_IP="0.0.0.0", TLS_CERT_FILE="/provided.crt"), True, True, "192.168.201.237")

    def test_shared_dns_identity_uses_alpn_without_ambiguous_sni_fallback(self):
        for host, dev_ca in (("neko.localhost", True), ("rooms.real-domain.org", False)):
            values = settings(APP_HOST=host, TURN_HOST=host)
            demo.validate_topology(values, dev_ca, public=False)
            self.assertEqual(values["WEB_SNI_ROUTE"], "")
            self.assertEqual(values["DEV_CA_LOCATION"], "")
        values = settings()
        demo.validate_topology(values, True, False)
        self.assertEqual(values["WEB_SNI_ROUTE"], "neko.localhost 127.0.0.1:8443;")

    def test_alpn_http_ip_fast_path_and_ambiguous_tls_classifier(self):
        nginx = (HERE / "nginx.conf.template").read_text()
        self.assertIn("map $ssl_preread_server_name $sni_backend", nginx)
        self.assertIn("@WEB_SNI_ROUTE@", nginx)
        self.assertIn("map $ssl_preread_alpn_protocols $tls_backend", nginx)
        self.assertIn('"" $sni_backend;', nginx)
        self.assertIn("default 127.0.0.1:5349;", nginx)
        http_alpn = re.search(r"\n\s+~(\S+) 127.0.0.1:8443;", nginx).group(1)
        for offered in ("h2", "http/1.1", "h2,http/1.1", "stun.turn,http/1.1"):
            self.assertRegex(offered, http_alpn)
        for offered in ("", "stun.turn", "stun.nat-discovery", "h2c", "not-h2", "http/1x1", "http/1.10"):
            self.assertNotRegex(offered, http_alpn)
        self.assertIn("map $ssl_preread_protocol $entry_backend", nginx)
        self.assertIn('"" 127.0.0.1:8081;', nginx)
        self.assertIn("proxy_pass $entry_backend;", nginx)
        self.assertIn("listen 443;\n        ssl_preread on;", nginx)
        self.assertNotIn("listen 443 ssl", nginx)
        self.assertIn("listen 127.0.0.1:8081;", nginx)
        self.assertIn("listen 127.0.0.1:8443 ssl;", nginx)
        self.assertIn("listen 127.0.0.1:5349 ssl;", nginx)
        self.assertEqual(nginx.count("resolver 127.0.0.11 valid=10s ipv6=off;"), 2)
        self.assertIn("map $remote_addr $classifier_backend { default classifier:3479; }", nginx)
        self.assertIn("proxy_pass $classifier_backend;", nginx)
        self.assertNotIn("turn:3478", nginx, "ambiguous TLS must be classified, never sent directly to TURN")
        self.assertIn("listen 8082;", nginx)
        self.assertNotIn("listen 8082 ssl", nginx)
        self.assertIn("set $manager http://neko-rooms:8080;", nginx)

    def test_classifier_accepts_only_positive_http_or_complete_stun_header(self):
        classifier = (HERE / "haproxy.cfg.template").read_text()
        for setting in ("mode tcp", "bind :3479", "tcp-request inspect-delay 5s",
                        "acl is_http req.proto_http", "acl stun_header req.len ge 20",
                        "acl stun_cookie req.payload(4,4) -m bin 2112a442",
                        "tcp-request content accept if is_http",
                        "tcp-request content accept if stun_header stun_cookie",
                        "tcp-request content reject if WAIT_END",
                        "use_backend https_backend if is_http",
                        "use_backend turn_backend if stun_header stun_cookie",
                        "nameserver embedded 127.0.0.11:53",
                        "server gateway gateway:8082 resolvers docker resolve-prefer ipv4 init-addr none",
                        "server turn turn:3478 resolvers docker resolve-prefer ipv4 init-addr none"):
            self.assertIn(setting, classifier)
        self.assertNotIn("default_backend", classifier, "unknown traffic must not silently become TURN")
        self.assertNotRegex(classifier, r"(?m)^\s*(?:bind .*ssl|crt |ca-file )")

    def test_private_http_backend_preserves_https_origin_and_public_host(self):
        nginx = (HERE / "nginx.conf.template").read_text()
        bootstrap, web = nginx.split("listen 127.0.0.1:8443 ssl;", 1)
        self.assertIn("listen 8082;", web)
        self.assertNotIn("proxy_pass $manager", bootstrap, "public plaintext is CA/redirect only")
        for header in ("Host $http_host", "X-Forwarded-Host $http_host", "X-Forwarded-Proto https",
                       "X-Forwarded-Port @PUBLIC_PORT@", "Authorization $http_authorization",
                       "Upgrade $http_upgrade", "Connection $connection_upgrade"):
            self.assertIn("proxy_set_header " + header + ";", web)
        self.assertNotIn("proxy_set_header X-Forwarded-Proto $scheme;", web)

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

    def configure(self, **options):
        return demo.configure(self.env, self.state, dev_ca=True, **options)

    def configure_lan(self, **options):
        self.values["PUBLIC_BIND_IP"] = "0.0.0.0"
        self.write_settings()
        return self.configure(public=True, lan_ip="192.168.201.237", **options)

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
                expected_mode = 0o644 if item.name == "haproxy.cfg" else 0o600
                self.assertEqual(stat.S_IMODE(item.stat().st_mode), expected_mode, item.name)
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

    def test_lan_leaf_has_ip_san_and_checks_ip_not_dns(self):
        self.configure_lan()
        cert, key = self.state / "tls/server.crt", self.state / "tls/server.key"
        demo.run_openssl("verify", "-CAfile", str(self.state / "tls/ca.crt"), "-verify_ip", "192.168.201.237", str(cert))
        with mock.patch.object(demo, "run_openssl", wraps=demo.run_openssl) as openssl:
            demo.check_certificate(cert, key, {"APP_HOST": "192.168.201.237", "TURN_HOST": "192.168.201.237"})
        self.assertEqual(sum("-checkip" in call.args for call in openssl.call_args_list), 1)
        self.assertFalse(any("-checkhost" in call.args for call in openssl.call_args_list))
        with self.assertRaises(ValueError):
            demo.check_certificate(cert, key, {"APP_HOST": "192.168.201.238", "TURN_HOST": "192.168.201.238"})
        # A DNS SAN containing an IP's text is NOT a valid IP identity.
        dns_cert, dns_key = self.root / "dns-only.crt", self.root / "dns-only.key"
        demo.run_openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                         "-subj", "/CN=192.168.201.237", "-addext", "subjectAltName=DNS:192.168.201.237",
                         "-keyout", str(dns_key), "-out", str(dns_cert))
        with self.assertRaises(ValueError):
            demo.check_certificate(dns_cert, dns_key, {"APP_HOST": "192.168.201.237", "TURN_HOST": "192.168.201.237"})

    def test_explicit_leaf_renewal_migrates_to_lan_without_rotating_private_state(self):
        self.configure()
        previous = {name: (self.state / name).read_bytes() for name in
                    ("credentials.json", "tls/ca.crt", "tls/ca.key", "tls/server.key", "room-request.json")}
        old_leaf = (self.state / "tls/server.crt").read_bytes()
        (self.state / "storage" / "owned-data").write_text("keep profile and downloads")
        with self.assertRaises(ValueError):
            self.configure_lan()
        self.assertEqual((self.state / "tls/server.crt").read_bytes(), old_leaf)
        self.configure_lan(renew_server_cert=True)
        self.assertNotEqual((self.state / "tls/server.crt").read_bytes(), old_leaf)
        self.assertTrue(all((self.state / name).read_bytes() == content for name, content in previous.items()),
                        "LAN migration must preserve existing CA, key, every credential and room payload")
        self.assertEqual((self.state / "storage" / "owned-data").read_text(), "keep profile and downloads")
        manager = dict(line.split("=", 1) for line in (self.state / "manager.env").read_text().splitlines())
        self.assertEqual(manager["NEKO_ROOMS_ADMIN_PASSWORD"], self.values["OWNER_PASSWORD"])
        self.assertEqual(manager["NEKO_ROOMS_ADMIN_USERNAME"], self.values["OWNER_USERNAME"])
        ice = json.loads(manager["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])[0]
        saved = json.loads(previous["credentials.json"])
        self.assertEqual(ice["urls"], ["turns:192.168.201.237:8443?transport=tcp"])
        self.assertEqual((ice["username"], ice["credential"]), (saved["turn_username"], saved["turn_password"]))
        repeated = {path: path.read_bytes() for path in self.state.rglob("*") if path.is_file()}
        self.configure_lan()
        self.assertTrue(all(path.read_bytes() == content for path, content in repeated.items()),
                        "repeat LAN configure without explicit renewal must be idempotent")

    def test_lan_turn_fqdn_renewal_covers_both_identities_without_private_state_rotation(self):
        self.configure_lan()
        previous = {name: (self.state / name).read_bytes() for name in
                    ("credentials.json", "tls/ca.crt", "tls/ca.key", "tls/server.key", "public/ca.crt", "room-request.json")}
        old_leaf = (self.state / "tls/server.crt").read_bytes()
        (self.state / "storage" / "owned-data").write_text("retain profile and downloads")
        with self.assertRaises(ValueError):
            self.configure_lan(turn_host=LAN_TURN_HOST)
        self.assertEqual((self.state / "tls/server.crt").read_bytes(), old_leaf)
        self.configure_lan(turn_host=LAN_TURN_HOST, renew_server_cert=True)
        self.assertNotEqual((self.state / "tls/server.crt").read_bytes(), old_leaf)
        self.assertTrue(all((self.state / name).read_bytes() == content for name, content in previous.items()),
                        "adding a TURN FQDN must not rotate CA, server key, public CA copy, any credential or room payload")
        cert, key = self.state / "tls/server.crt", self.state / "tls/server.key"
        for check, identity in (("-verify_ip", "192.168.201.237"), ("-verify_hostname", LAN_TURN_HOST)):
            demo.run_openssl("verify", "-CAfile", str(self.state / "tls/ca.crt"), check, identity, str(cert))
        with mock.patch.object(demo, "run_openssl", wraps=demo.run_openssl) as openssl:
            demo.check_certificate(cert, key, {"APP_HOST": "192.168.201.237", "TURN_HOST": LAN_TURN_HOST})
        self.assertEqual(sum("-checkip" in call.args for call in openssl.call_args_list), 1)
        self.assertEqual(sum("-checkhost" in call.args for call in openssl.call_args_list), 1)
        manager = dict(line.split("=", 1) for line in (self.state / "manager.env").read_text().splitlines())
        self.assertEqual(manager["NEKO_ROOMS_ADMIN_PASSWORD"], self.values["OWNER_PASSWORD"])
        self.assertEqual(manager["NEKO_ROOMS_ADMIN_USERNAME"], self.values["OWNER_USERNAME"])
        ice = json.loads(manager["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])[0]
        saved = json.loads(previous["credentials.json"])
        self.assertEqual(ice["urls"], [f"turns:{LAN_TURN_HOST}:8443?transport=tcp"])
        self.assertEqual((ice["username"], ice["credential"]), (saved["turn_username"], saved["turn_password"]))
        self.assertIn("realm=" + LAN_TURN_HOST + "\n", (self.state / "turnserver.conf").read_text())
        nginx = (self.state / "nginx.conf").read_text()
        self.assertIn('"192.168.201.237:8443" 1;', nginx)
        self.assertIn("return 308 https://192.168.201.237:8443$request_uri;", nginx)
        self.assertIn("proxy_set_header Host $http_host;", nginx)
        self.assertIn("proxy_set_header X-Forwarded-Port 8443;", nginx)
        self.assertNotIn(LAN_TURN_HOST + " 127.0.0.1:8443;", nginx, "TURN SNI must not select HTTPS")
        self.assertEqual((self.state / "storage" / "owned-data").read_text(), "retain profile and downloads")
        repeated = {path: path.read_bytes() for path in self.state.rglob("*") if path.is_file()}
        self.configure_lan(turn_host=LAN_TURN_HOST)
        self.assertTrue(all(path.read_bytes() == content for path, content in repeated.items()),
                        "repeat mixed-identity configure must preserve all generated bytes without renewal")

    def test_turn_host_without_lan_fails_before_creating_state(self):
        with self.assertRaisesRegex(ValueError, "--turn-host requires"):
            self.configure(turn_host=LAN_TURN_HOST)
        self.assertFalse(self.state.exists())

    def test_cli_passes_explicit_lan_turn_host_and_leaf_renewal(self):
        arguments = ["demo.py", "configure", "--env-file", str(self.env), "--state-dir", str(self.state),
                     "--lan-ip", "192.168.201.237", "--turn-host", LAN_TURN_HOST,
                     "--dev-ca", "--public", "--renew-server-cert"]
        with mock.patch.object(demo.sys, "argv", arguments), mock.patch.object(
                demo, "configure", return_value=self.state) as configure, mock.patch("builtins.print"):
            self.assertEqual(demo.main(), 0)
        configure.assert_called_once_with(self.env, self.state, True, True, "192.168.201.237", True, LAN_TURN_HOST)

    def test_leaf_renewal_never_replaces_ca_or_supplied_certificates(self):
        with self.assertRaisesRegex(ValueError, "requires --dev-ca"):
            demo.configure(self.env, self.state, renew_server_cert=True)
        self.assertFalse(self.state.exists())
        self.configure()
        previous_ca = (self.state / "tls/ca.crt").read_bytes()
        (self.state / "tls/server.crt").unlink()
        with self.assertRaisesRegex(ValueError, "incomplete existing development CA"):
            self.configure(renew_server_cert=True)
        self.assertEqual((self.state / "tls/ca.crt").read_bytes(), previous_ca)
        self.assertFalse((self.state / "tls/server.crt").exists())

    def test_lan_http_bootstrap_serves_only_public_ca_and_redirects_to_same_authority(self):
        self.configure()
        self.assertNotIn("location = /ca.crt", (self.state / "nginx.conf").read_text())
        self.assertFalse(list((self.state / "public").iterdir()))
        self.configure_lan(renew_server_cert=True)
        public = self.state / "public"
        self.assertEqual([path.name for path in public.iterdir()], ["ca.crt"])
        self.assertEqual((public / "ca.crt").read_bytes(), (self.state / "tls/ca.crt").read_bytes())
        self.assertEqual(stat.S_IMODE(public.stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE((public / "ca.crt").stat().st_mode), 0o644)
        for name in ("tls/ca.key", "tls/ca.crt", "tls/server.key", "credentials.json", "manager.env", "room-request.json"):
            self.assertEqual(stat.S_IMODE((self.state / name).stat().st_mode), 0o600, name)
        nginx = (self.state / "nginx.conf").read_text()
        self.assertIn("location = /ca.crt { alias /run/relay/public/ca.crt;", nginx)
        self.assertIn("return 308 https://192.168.201.237:8443$request_uri;", nginx)
        self.assertIn('"192.168.201.237:8443" 1;', nginx)
        self.assertIn("if ($valid_host = 0) { return 421; }", nginx)
        self.assertIn("if ($valid_http_host = 0) { return 421; }", nginx)
        self.assertNotIn("192.168.201.237 127.0.0.1:8443;", nginx)
        self.assertNotRegex(nginx, r"alias .*?(?:key|credentials|manager\.env)")
        for header in ("Host $http_host", "X-Forwarded-Host $http_host", "X-Forwarded-Proto https", "X-Forwarded-Port 8443"):
            self.assertIn("proxy_set_header " + header + ";", nginx)

    def test_lan_bootstrap_authority_uses_http_port_rules_not_https_port_rules(self):
        for selected, http_authority, https_authority in (
                ("443", "192.168.201.237:443", "192.168.201.237"),
                ("80", "192.168.201.237", "192.168.201.237:80")):
            with self.subTest(port=selected):
                self.values["PUBLIC_PORT"] = selected
                self.configure_lan()
                nginx = (self.state / "nginx.conf").read_text()
                self.assertIn(f'map $http_host $valid_http_host {{ default 0; "{http_authority}" 1; }}', nginx)
                self.assertIn("return 308 https://" + https_authority + "$request_uri;", nginx)

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
        for name in ("turn", "neko-rooms", "classifier"):
            self.assertFalse(services[name].get("ports"))
            self.assertNotEqual(services[name].get("network_mode"), "host")
        classifier = services["classifier"]
        self.assertRegex(classifier["image"], r"^haproxy:3\.2-alpine@sha256:[a-f0-9]{64}$")
        self.assertEqual(classifier["user"], "haproxy")
        self.assertEqual(classifier["cap_drop"], ["ALL"])
        self.assertTrue(classifier["read_only"])
        self.assertIn("classifier", services["gateway"]["depends_on"])
        self.assertFalse(classifier.get("depends_on"), "runtime DNS must break the gateway dependency cycle")
        self.assertEqual(len(classifier["volumes"]), 1, "classifier must not receive TLS or auth material")
        mount = classifier["volumes"][0]
        self.assertEqual(mount["source"], str(self.state / "haproxy.cfg"))
        self.assertEqual(mount["target"], "/usr/local/etc/haproxy/haproxy.cfg")
        self.assertTrue(mount["read_only"])
        self.assertFalse(mount["bind"]["create_host_path"])
        self.assertEqual((self.state / "haproxy.cfg").read_text(), (HERE / "haproxy.cfg.template").read_text())
        for secret in json.loads((self.state / "credentials.json").read_text()).values():
            self.assertNotIn(secret, (self.state / "haproxy.cfg").read_text())
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

    def test_custom_cdp_image_is_identical_in_payload_and_owner_allowlist(self):
        if not shutil.which("docker") or subprocess.run(["docker", "compose", "version"], capture_output=True).returncode:
            self.skipTest("Docker Compose unavailable; default image contracts still run")
        self.values["ROOM_IMAGE"] = "local-native-cdp:isolated-build"
        self.write_settings()
        self.configure()
        request = json.loads((self.state / "room-request.json").read_text())
        environment = {k: v for k, v in os.environ.items() if k not in demo.COMPOSE_KEYS}
        result = subprocess.run(demo.compose_command(self.state, ["config", "--format", "json"]),
                                env=environment, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, "Compose config rejected custom image (output withheld)")
        project = json.loads(result.stdout.replace("$$", "$"))
        allowed = project["services"]["neko-rooms"]["environment"]["NEKO_ROOMS_NEKO_IMAGES"]
        self.assertEqual(request["neko_image"], self.values["ROOM_IMAGE"])
        self.assertEqual(allowed, request["neko_image"])

    def test_actual_lan_compose_keeps_native_relay_one_tcp_port_and_literal_auth(self):
        if not shutil.which("docker") or subprocess.run(["docker", "compose", "version"], capture_output=True).returncode:
            self.skipTest("Docker Compose unavailable; source-only contracts still run")
        environment = {k: v for k, v in os.environ.items() if k not in demo.COMPOSE_KEYS}
        for turn_host in (None, LAN_TURN_HOST):
            with self.subTest(turn_host=turn_host):
                self.configure_lan(turn_host=turn_host, renew_server_cert=turn_host is not None)
                result = subprocess.run(demo.compose_command(self.state, ["config", "--format", "json"]),
                                        env=environment, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, "Compose config rejected LAN input (output withheld)")
                services = json.loads(result.stdout.replace("$$", "$"))["services"]
                ports = services["gateway"]["ports"]
                self.assertEqual(len(ports), 1)
                self.assertEqual((ports[0]["host_ip"], ports[0]["published"], ports[0]["target"], ports[0]["protocol"]),
                                 ("0.0.0.0", "8443", 443, "tcp"))
                for name, service in services.items():
                    if name != "gateway":
                        self.assertFalse(service.get("ports"), name)
                env = services["neko-rooms"]["environment"]
                self.assertEqual(env["NEKO_ROOMS_NETWORK_MODE"], "relay")
                self.assertEqual(env["NEKO_ROOMS_NEKO_IMAGES"], demo.ROOM_IMAGE)
                self.assertEqual(env["NEKO_ROOMS_INSTANCE_URL"], "https://192.168.201.237:8443/")
                self.assertEqual(env["NEKO_ROOMS_AGENT_PUBLIC_URL"], "https://192.168.201.237:8443/rooms/")
                self.assertEqual(env["NEKO_ROOMS_ADMIN_USERNAME"], self.values["OWNER_USERNAME"])
                self.assertEqual(env["NEKO_ROOMS_ADMIN_PASSWORD"], self.values["OWNER_PASSWORD"])
                self.assertEqual(json.loads(env["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])[0]["urls"],
                                 [f"turns:{turn_host or '192.168.201.237'}:8443?transport=tcp"])
                mounts = services["gateway"]["volumes"]
                public_mount = next(v for v in mounts if v["target"] == "/run/relay/public")
                self.assertEqual(public_mount["source"], str(self.state / "public"))
                self.assertTrue(public_mount["read_only"])
                self.assertFalse(any(v["source"].endswith(("ca.key", "credentials.json", "manager.env")) for v in mounts))


if __name__ == "__main__":
    unittest.main(verbosity=2)
