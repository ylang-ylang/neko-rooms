#!/usr/bin/env python3
"""Opt-in real-container ingress tests; never attach to an existing deployment.

NEKO_RELAY_DOCKER_TESTS=1 python3 -B -m unittest discover \
    -s examples/single-port-relay -p test_ingress.py -v

Requires the pinned gateway/classifier/coturn images and the example's built
ROOM_IMAGE locally (used only as a Python fixture, not as a native room). No
images are pulled. The fixture publishes one random LOOPBACK TCP port, creates
its own Docker network, CA and credentials, then removes only its own resources.
This proves ingress transport, not native-browser/media or real CDP behavior.
"""
import base64
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest

HERE = Path(__file__).resolve().parent
if sys.argv[1:] != ["--backend"]:
    spec = importlib.util.spec_from_file_location("ingress_demo", HERE / "demo.py")
    demo = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(demo)
COOKIE = bytes.fromhex("2112a442")


class FixtureBackend(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == "/room/native/ws":
            if self.headers.get("Authorization") != "Bearer fixture-only":
                self.send_error(401)
                return
            key = self.headers["Sec-WebSocket-Key"]
            accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", accept)
            self.end_headers()
            # Send a small frame after the upgrade to prove it is not parsed
            # again as HTTP or STUN. The fixture is not Neko authentication.
            self.wfile.write(b"\x81\x07fixture")
            self.wfile.flush()
            self.close_connection = True
            return
        body = json.dumps({name: self.headers.get(name) for name in (
            "Host", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "Origin", "Authorization")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    @staticmethod
    def serve():
        def echo_udp():
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                udp.bind(("0.0.0.0", 3480))
                while True:
                    payload, peer = udp.recvfrom(65535)
                    udp.sendto(payload, peer)
        threading.Thread(target=echo_udp, daemon=True).start()
        ThreadingHTTPServer(("0.0.0.0", 8080), FixtureBackend).serve_forever()


@unittest.skipUnless(os.environ.get("NEKO_RELAY_DOCKER_TESTS") == "1", "opt-in isolated Docker ingress tests")
class IngressTests(unittest.TestCase):
    HOST = "127.0.0.1"

    @classmethod
    def command(cls, *args, **kwargs):
        result = subprocess.run(args, capture_output=True, text=True, timeout=60, **kwargs)
        if result.returncode:
            raise AssertionError(f"{args[0]} command failed: {result.stderr[-1000:]}")
        return result.stdout

    @classmethod
    def compose(cls, *args):
        return cls.command("docker", "compose", "--project-name", cls.name, "-f", str(cls.project_file), *args)

    @classmethod
    def setUpClass(cls):
        if not all(shutil.which(executable) for executable in ("docker", "node", "openssl")):
            raise unittest.SkipTest("Docker, Node and OpenSSL are required")
        cls.name = "neko-ingress-test-" + secrets.token_hex(5)
        cls.temporary = tempfile.TemporaryDirectory(prefix=cls.name + "-")
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.state = Path(cls.temporary.name)
        cls.project_file = cls.state / "project.json"
        cls.command("docker", "network", "create", "--driver", "bridge", cls.name)
        cls.addClassCleanup(lambda: cls.command("docker", "network", "rm", cls.name))
        network = json.loads(cls.command("docker", "network", "inspect", cls.name))[0]
        subnet = ipaddress.ip_network(network["IPAM"]["Config"][0]["Subnet"])
        with socket.socket() as available:
            available.bind(("127.0.0.1", 0))
            cls.port = available.getsockname()[1]
        cls.authority = cls.HOST + ":" + str(cls.port)
        cls.origin = "https://" + cls.authority
        values = dict(demo.DEFAULTS, MANAGER_IMAGE=demo.ROOM_IMAGE, ROOM_NETWORK=cls.name,
                      ROOM_NETWORK_CIDR=str(subnet), TURN_PRIVATE_IP=str(subnet.network_address + 10),
                      INSTANCE_NAME=cls.name, PUBLIC_PORT=str(cls.port))
        if cls.HOST != "127.0.0.1":
            values.update(APP_HOST=cls.HOST, TURN_HOST=cls.HOST)
        demo.validate_topology(values, dev_ca=True, public=False)
        if cls.HOST == "127.0.0.1":
            # The CLI limits dev LAN mode to RFC1918. Only this isolated fixture
            # issues a loopback-IP certificate for ordinary no-SNI IP clients.
            values.update(APP_HOST=cls.HOST, TURN_HOST=cls.HOST, WEB_SNI_ROUTE="",
                          PUBLIC_AUTHORITY=cls.authority, HTTP_AUTHORITY=cls.authority, PUBLIC_ORIGIN=cls.origin)
        if values["WEB_SNI_ROUTE"]:
            raise AssertionError("shared identities must use post-TLS classification, not web SNI")
        values["DEV_CA_LOCATION"] = "location = /ca.crt { alias /run/relay/public/ca.crt; }"
        cert, key = demo.development_cert(cls.state, values)
        cls.ca = cls.state / "tls/ca.crt"
        if cls.HOST != "127.0.0.1":
            cls.command("openssl", "verify", "-CAfile", str(cls.ca), "-verify_hostname", cls.HOST, str(cert))
            sans = cls.command("openssl", "x509", "-in", str(cert), "-noout", "-ext", "subjectAltName")
            if sans.count("DNS:" + cls.HOST) != 1:
                raise AssertionError("shared hostname certificate SAN must cover the name exactly once")
        cls.turn_ip = values["TURN_PRIVATE_IP"]
        cls.turn_user, cls.turn_password = "fixture", secrets.token_hex(32)
        values.update(TURN_USERNAME=cls.turn_user, TURN_PASSWORD=cls.turn_password)
        for name in ("nginx.conf", "haproxy.cfg", "turnserver.conf"):
            content = (HERE / (name + ".template")).read_text()
            (cls.state / name).write_text(re.sub(r"@([A-Z_]+)@", lambda m: values[m[1]], content))
        (cls.state / "haproxy.cfg").chmod(0o644)
        public = cls.state / "public"
        public.mkdir(mode=0o755)
        (public / "ca.crt").write_bytes(cls.ca.read_bytes())
        (public / "ca.crt").chmod(0o644)
        (cls.state / "storage").mkdir()
        (cls.state / "manager.env").write_text("")
        values.update(COMPOSE_PROJECT_NAME=cls.name, RELAY_STATE_DIR=str(cls.state),
                      HAPROXY_CONFIG_FILE=str(cls.state / "haproxy.cfg"), TLS_CERT_FILE=str(cert),
                      TLS_KEY_FILE=str(key), STORAGE_PATH=str(cls.state / "storage"))
        env = {**os.environ, **{k: values[k] for k in demo.COMPOSE_KEYS}}
        project = json.loads(cls.command("docker", "compose", "-f", str(HERE / "compose.yaml"),
                                         "config", "--format", "json", env=env))
        # Keep the exact production gateway, classifier and coturn. Substitute
        # only Manager with a byte/forwarding fixture on the same private name.
        project["services"]["neko-rooms"] = {
            "image": demo.ROOM_IMAGE, "pull_policy": "never", "entrypoint": ["python3"],
            "command": ["/fixture.py", "--backend"], "user": "0:0", "networks": ["rooms"],
            "volumes": [{"type": "bind", "source": str(HERE / "test_ingress.py"),
                         "target": "/fixture.py", "read_only": True}],
        }
        cls.project_file.write_text(json.dumps(project))
        cls.addClassCleanup(lambda: cls.compose("down", "--timeout", "2"))
        cls.compose("run", "--rm", "--no-deps", "classifier", "-c", "-f", "/usr/local/etc/haproxy/haproxy.cfg")
        # Explicitly start classifier before gateway DNS exists. Its init-addr
        # none must permit this, then discover the backend once gateway starts.
        cls.compose("up", "-d", "--pull", "never", "classifier", "turn", "neko-rooms")
        cls.compose("up", "-d", "--pull", "never", "gateway")
        # Bounded readiness is part of this synchronous test, not a background
        # session poll. Once ready, no request retry masks a routing failure.
        deadline = time.monotonic() + 15
        while True:
            try:
                result = cls.node_get()
                if result["status"] == 200:
                    break
            except (AssertionError, json.JSONDecodeError):
                pass
            if time.monotonic() > deadline:
                raise AssertionError("isolated gateway did not become ready")
            time.sleep(0.2)
        container = cls.compose("ps", "-q", "neko-rooms").strip()
        details = json.loads(cls.command("docker", "inspect", container))[0]
        cls.peer_ip = details["NetworkSettings"]["Networks"][cls.name]["IPAddress"]

    @classmethod
    def node_get(cls, alpn=False):
        script = r"""
const https = require('https'), fs = require('fs');
const options = {ca: fs.readFileSync(process.argv[2]), headers: {
  Origin: process.argv[1], Authorization: 'Bearer fixture-only'}};
// Test-local DNS only; TLS SNI and certificate hostname verification stay normal.
if (new URL(process.argv[1]).hostname !== '127.0.0.1')
  options.lookup = (_host, opts, cb) => cb(null, opts.all
    ? [{address: '127.0.0.1', family: 4}] : '127.0.0.1', 4);
if (process.argv[3] === 'alpn') options.ALPNProtocols = ['http/1.1'];
https.get(process.argv[1] + '/manage/auth/status', options, res => {
  const alpn = res.socket.alpnProtocol || false, servername = res.socket.servername || false;
  let body = ''; res.setEncoding('utf8'); res.on('data', d => body += d);
  res.on('end', () => console.log(JSON.stringify({status: res.statusCode, body, alpn, servername})));
}).on('error', e => { console.error(e.code); process.exitCode = 1; });
"""
        return json.loads(cls.command("node", "-e", script, cls.origin, str(cls.ca), "alpn" if alpn else "default"))

    def tls_connection(self, alpn=None):
        context = ssl.create_default_context(cafile=str(self.ca))
        if alpn:
            context.set_alpn_protocols(alpn)
        plain = socket.create_connection(("127.0.0.1", self.port), timeout=10)
        # Normal DNS SNI/hostname verification for the shared-name case; no SNI
        # for the IP case. Neither changes the TLS application protocol.
        return context.wrap_socket(plain, server_hostname=self.HOST)

    @staticmethod
    def receive_exact(connection, length):
        result = b""
        while len(result) < length:
            data = connection.recv(length - len(result))
            if not data:
                raise AssertionError("unexpected transport EOF")
            result += data
        return result

    @staticmethod
    def attribute(kind, value):
        return struct.pack("!HH", kind, len(value)) + value + b"\0" * (-len(value) % 4)

    @classmethod
    def stun_request(cls, connection, kind, attributes=b"", key=None):
        transaction = secrets.token_bytes(12)
        length = len(attributes) + (24 if key else 0)
        header = struct.pack("!HH", kind, length) + COOKIE + transaction
        packet = header + attributes
        if key:
            packet += cls.attribute(8, hmac.new(key, packet, hashlib.sha1).digest())
        connection.sendall(packet)
        response = cls.receive_exact(connection, 20)
        assert response[4:8] == COOKIE and response[8:20] == transaction
        body = cls.receive_exact(connection, struct.unpack("!H", response[2:4])[0])
        parsed, offset = {}, 0
        while offset < len(body):
            attr, size = struct.unpack("!HH", body[offset:offset + 4])
            parsed[attr] = body[offset + 4:offset + 4 + size]
            offset += 4 + (size + 3) // 4 * 4
        return struct.unpack("!H", response[:2])[0], parsed

    def test_default_node_https_and_alpn_fast_path_forward_https_headers(self):
        for explicit_alpn in (False, True):
            with self.subTest(explicit_alpn=explicit_alpn):
                result = self.node_get(explicit_alpn)
                self.assertEqual(result["status"], 200)
                self.assertEqual(result["servername"], False if self.HOST == "127.0.0.1" else self.HOST)
                if not explicit_alpn:
                    self.assertFalse(result["alpn"])
                headers = json.loads(result["body"])
                self.assertEqual(headers, {
                    "Host": self.authority, "X-Forwarded-Host": self.authority,
                    "X-Forwarded-Proto": "https", "X-Forwarded-Port": str(self.port),
                    "Origin": self.origin, "Authorization": "Bearer fixture-only"})

    def test_no_alpn_websocket_authenticated_fixture_and_post_upgrade_bytes(self):
        for authorization, status in (("", 401), ("Bearer fixture-only", 101)):
            with self.subTest(authorized=bool(authorization)), self.tls_connection() as connection:
                request = (f"GET /room/native/ws HTTP/1.1\r\nHost: {self.authority}\r\n"
                           "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
                           "Sec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==\r\n"
                           f"Authorization: {authorization}\r\nOrigin: {self.origin}\r\n\r\n")
                connection.sendall(request.encode())
                response = b""
                while b"\r\n\r\n" not in response:
                    response += self.receive_exact(connection, 1)
                self.assertIn(f" {status} ".encode(), response.split(b"\r\n", 1)[0])
                if status == 101:
                    self.assertEqual(self.receive_exact(connection, 9), b"\x81\x07fixture")

    def test_fragmented_no_alpn_turn_allocation_and_channel_data(self):
        with self.tls_connection() as connection:
            # Split before the cookie/header completes: classification must wait.
            tx = secrets.token_bytes(12)
            packet = struct.pack("!HH", 3, 8) + COOKIE + tx + self.attribute(0x19, b"\x11\0\0\0")
            connection.sendall(packet[:7])
            connection.sendall(packet[7:19])
            connection.sendall(packet[19:])
            header = self.receive_exact(connection, 20)
            self.assertEqual(header[:2], b"\x01\x13")
            body = self.receive_exact(connection, struct.unpack("!H", header[2:4])[0])
            attributes, offset = {}, 0
            while offset < len(body):
                kind, size = struct.unpack("!HH", body[offset:offset + 4])
                attributes[kind] = body[offset + 4:offset + 4 + size]
                offset += 4 + (size + 3) // 4 * 4
            self.assertEqual(attributes[9][2:4], b"\x04\x01", "TURN must retain long-term authentication")
            realm, nonce = attributes[0x14], attributes[0x15]
            self.assertEqual(realm.decode(), self.HOST)
            key = hashlib.md5(self.turn_user.encode() + b":" + realm + b":" + self.turn_password.encode()).digest()
            auth = self.attribute(6, self.turn_user.encode()) + self.attribute(0x14, realm) + self.attribute(0x15, nonce)
            kind, allocated = self.stun_request(connection, 3, self.attribute(0x19, b"\x11\0\0\0") + auth, key)
            self.assertEqual(kind, 0x0103)
            address = bytes(a ^ b for a, b in zip(allocated[0x16][4:8], COOKIE))
            self.assertEqual(socket.inet_ntoa(address), self.turn_ip)
            peer = b"\0\x01" + struct.pack("!H", 3480 ^ 0x2112) + bytes(
                a ^ b for a, b in zip(socket.inet_aton(self.peer_ip), COOKIE))
            channel = self.attribute(0x0C, b"\x40\x01\0\0") + self.attribute(0x12, peer)
            kind, _ = self.stun_request(connection, 9, channel + auth, key)
            self.assertEqual(kind, 0x0109)
            payload = b"TURN-channel-data"
            connection.sendall(struct.pack("!HH", 0x4001, len(payload)) + payload + b"\0" * (-len(payload) % 4))
            channel, size = struct.unpack("!HH", self.receive_exact(connection, 4))
            self.assertEqual(channel, 0x4001)
            self.assertEqual(self.receive_exact(connection, size), payload)

    def test_unknown_or_incomplete_initial_payload_is_closed_not_routed(self):
        for payload in (b"unrecognized-first-protocol\r\n", b"\0\x01\0\0" + COOKIE):
            with self.subTest(payload=payload.hex()), self.tls_connection() as connection:
                connection.sendall(payload)
                started = time.monotonic()
                self.assertEqual(connection.recv(1), b"", "unknown traffic must not get an HTTP/STUN response")
                self.assertLess(time.monotonic() - started, 7)

    def test_plaintext_bootstrap_only_ca_or_https_redirect(self):
        import http.client
        from contextlib import closing
        with closing(http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)) as connection:
            connection.request("GET", "/ca.crt", headers={"Host": self.authority})
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.read(), self.ca.read_bytes())
            connection.request("GET", "/manage/auth/status", headers={"Host": self.authority})
            response = connection.getresponse()
            self.assertEqual(response.status, 308)
            self.assertEqual(response.getheader("Location"), self.origin + "/manage/auth/status")
            response.read()


class SharedNameIngressTests(IngressTests):
    # One FQDN, one certificate name, one socket; no web-SNI fallback. Reuse all
    # the ordinary HTTP/WS, STUN allocation/ChannelData and rejection checks.
    HOST = "shared.neko.localhost"


if __name__ == "__main__":
    if sys.argv[1:] == ["--backend"]:
        FixtureBackend.serve()
    else:
        unittest.main(verbosity=2)
