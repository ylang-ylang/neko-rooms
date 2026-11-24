#!/usr/bin/env python3
"""Local example test only: scratch certs, loopback ports, no Neko or WAN changes."""
import argparse
import hashlib
import hmac
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import struct
import subprocess
import sys
import tempfile

import configure

HERE = Path(__file__).resolve().parent
PYTHON_IMAGE = "python:3.13-alpine@sha256:9ef1a599f863231c3d91e941389bfdc4a2d6070557243d54df1a4e57015b960e"
COOKIE = 0x2112A442


def run(*args, quiet=True):
    result = subprocess.run(args, capture_output=True, text=True, timeout=90)
    if result.returncode:
        # These commands never print rendered Compose/env/credential contents.
        raise RuntimeError(f"{args[0]} command failed: {result.stderr.strip()}")
    if not quiet:
        print(result.stdout.strip())
    return result.stdout


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def fixture_server():
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.headers.get("Upgrade", "").lower() == "websocket":
                self.send_response(101)
                self.send_header("Upgrade", "websocket")
                self.send_header("Connection", "Upgrade")
                self.end_headers()
                return
            data = json.dumps({"path": self.path, "headers": dict(self.headers),
                               "owner_hash": hashlib.sha256(os.environ.get("NEKO_ROOMS_ADMIN_PASSWORD", "").encode()).hexdigest()}).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        do_POST = do_GET

        def log_message(self, *_):
            pass
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def unused_subnet():
    ids = run("docker", "network", "ls", "-q").split()
    networks = json.loads(run("docker", "network", "inspect", *ids))
    occupied = [ipaddress.ip_network(c["Subnet"]) for n in networks for c in (n["IPAM"].get("Config") or []) if c.get("Subnet")]
    # Include host routes: do not displace an existing private VPN/LAN route.
    routes = json.loads(run("ip", "-j", "route", "show"))
    occupied += [ipaddress.ip_network(r["dst"]) for r in routes if r.get("dst") not in (None, "default")]
    for subnet in ipaddress.ip_network("172.29.0.0/16").subnets(new_prefix=24):
        if not any(subnet.overlaps(other) for other in occupied if other.version == 4):
            return subnet
    raise RuntimeError("no unused fixture /24; configure a local test network separately")


def attr(kind, value):
    return struct.pack("!HH", kind, len(value)) + value + b"\0" * (-len(value) % 4)


def packet(kind, attributes, credentials=None):
    txid = secrets.token_bytes(12)
    if credentials:
        username, realm, nonce, password = credentials
        attributes += attr(6, username.encode()) + attr(0x14, realm) + attr(0x15, nonce)
        key = hashlib.md5(username.encode() + b":" + realm + b":" + password.encode()).digest()
        header = struct.pack("!HHI12s", kind, len(attributes) + 24, COOKIE, txid)
        attributes += attr(8, hmac.new(key, header + attributes, hashlib.sha1).digest())
    return struct.pack("!HHI12s", kind, len(attributes), COOKIE, txid) + attributes


def receive(sock, count):
    data = b""
    while len(data) < count:
        chunk = sock.recv(count - len(data))
        if not chunk:
            raise RuntimeError("TURN closed the TLS stream before a complete response")
        data += chunk
    return data


def transact(sock, kind, attributes, credentials=None):
    request = packet(kind, attributes, credentials)
    sock.sendall(request)
    header = receive(sock, 20)
    response_kind, length, cookie, txid = struct.unpack("!HHI12s", header)
    require(cookie == COOKIE and txid == request[8:20], "not a matching STUN/TURN response")
    body = receive(sock, length)
    parsed, offset = {}, 0
    while offset < len(body):
        key, size = struct.unpack("!HH", body[offset:offset + 4])
        parsed[key] = body[offset + 4:offset + 4 + size]
        offset += 4 + size + (-size % 4)
    return response_kind, parsed


def error_code(attributes):
    value = attributes.get(9, b"\0\0\0\0")
    return (value[2] & 7) * 100 + value[3]


def peer_attribute(ip, port=9):
    return attr(0x12, struct.pack("!BBHI", 0, 1, port ^ (COOKIE >> 16), int(ipaddress.IPv4Address(ip)) ^ COOKIE))


def test_turn(context, host, port, state, subnet):
    secret = json.loads((state / "turn-credentials.json").read_text())
    def connect():
        return context.wrap_socket(socket.create_connection(("127.0.0.1", port), timeout=5), server_hostname=host)
    udp = attr(0x19, b"\x11\0\0\0")
    with connect() as sock:
        require(sock.version() in ("TLSv1.2", "TLSv1.3"), "TLS did not negotiate")
        kind, challenge = transact(sock, 3, udp)
        require(kind == 0x113 and error_code(challenge) == 401, "unauthenticated TURN allocation was not rejected")
        credentials = (secret["username"], challenge[0x14], challenge[0x15], secret["password"])
        wrong = (*credentials[:3], "wrong-password")
        kind, response = transact(sock, 3, udp, wrong)
        require(kind == 0x113 and error_code(response) == 401, "wrong TURN password was not rejected")
        kind, response = transact(sock, 3, udp, credentials)
        require(kind == 0x103, f"authenticated UDP allocation failed ({error_code(response)})")
        _, family, encoded_port, encoded_ip = struct.unpack("!BBHI", response[0x16])
        relay_ip = ipaddress.IPv4Address(encoded_ip ^ COOKIE)
        relay_port = encoded_port ^ (COOKIE >> 16)
        require(family == 1 and relay_ip == subnet.network_address + 2 and 49160 <= relay_port <= 49259,
                "TURN advertised something other than its private relay address/range")
        kind, response = transact(sock, 8, peer_attribute("1.1.1.1"), credentials)
        require(kind == 0x118 and error_code(response) == 403, "authorized TURN client can reach public UDP peers")
        kind, response = transact(sock, 8, peer_attribute(str(subnet.network_address + 100)), credentials)
        require(kind == 0x108, f"room-CIDR permission failed ({error_code(response)})")
        transact(sock, 4, attr(0x0d, struct.pack("!I", 0)), credentials)
    with connect() as sock:
        tcp = attr(0x19, b"\x06\0\0\0")
        kind, response = transact(sock, 3, tcp)
        if error_code(response) == 401:
            credentials = (secret["username"], response[0x14], response[0x15], secret["password"])
            kind, response = transact(sock, 3, tcp, credentials)
        require(kind == 0x113 and error_code(response) == 442, "TCP relay allocation was not disabled")
    print("PASS TURN/TLS: verified certificate; unauthenticated/wrong-password allocations rejected; authenticated allocation advertises private IP; public peer 403; private peer allowed; TCP relay 442")


def test_http(context, host, port, owner_password):
    authority = f"{host}:{port}"
    def request(path, headers=None, method="GET"):
        conn = http.client.HTTPSConnection("127.0.0.1", port, context=context, timeout=5)
        # Supply SNI/hostname verification explicitly while connecting to loopback.
        conn.sock = context.wrap_socket(socket.create_connection(("127.0.0.1", port), timeout=5), server_hostname=host)
        conn.request(method, path, headers={"Host": authority, **(headers or {})})
        response = conn.getresponse()
        result = response.status, dict(response.getheaders()), response.read()
        conn.close()
        return result
    for path, code in (("/", 302), ("/manage", 308), ("/unpublished", 404)):
        status, headers, _ = request(path)
        require(status == code, f"unexpected HTTP status for {path}")
        if code in (302, 308):
            require(headers["Location"] == "/manage/", "redirect lost external port/origin")
    for path in ("/manage/", "/manage/api/rooms", "/room/fixture/ws?x=1", "/rooms/fixture/json/version", "/rooms/fixture/json/list"):
        status, _, body = request(path, {"Authorization": "Bearer fixture-only", "X-Forwarded-Proto": "http", "Forwarded": "proto=http"})
        require(status == 200, "gateway added a global challenge or lost manager route")
        echoed = json.loads(body)
        require(echoed["owner_hash"] == hashlib.sha256(owner_password.encode()).hexdigest(), "raw env_file changed the running container's fixture owner password")
        headers = {k.lower(): v for k, v in echoed["headers"].items()}
        require(echoed["path"] == path and headers["host"] == authority, "gateway rewrote path or Host")
        require(headers["x-forwarded-proto"] == "https" and "forwarded" not in headers, "client spoofed trusted scheme")
        require(headers["authorization"] == "Bearer fixture-only", "gateway dropped Agent authorization")
    status, _, _ = request("/rooms/fixture/devtools/browser/id", {"Upgrade": "websocket", "Connection": "Upgrade", "Authorization": "Bearer fixture-only"})
    require(status == 101, "DevTools WebSocket upgrade did not reach the manager")
    require(request("/manage/", {"Host": "wrong.test"})[0] == 421, "unconfigured Host was accepted")
    print("PASS HTTPS: root redirect; management/Neko/Agent paths and Host preserved; HTTPS headers enforced; Bearer forwarded without global auth; DevTools Upgrade 101")


def validate():
    # No implicit pull, builds or use of a manager/Neko image.
    base = json.loads(run("docker", "image", "inspect", PYTHON_IMAGE))[0]
    require(base["Id"], "fixture Python image must be preloaded")
    subnet = unused_subnet()
    project = "neko-relay-fixture-" + secrets.token_hex(4)
    network_name = project + "-rooms"
    https, turns = free_port(), free_port()
    while turns == https:
        turns = free_port()
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="neko-relay-example-") as temporary:
        scratch = Path(temporary)
        state = scratch / "state"
        storage = scratch / "storage"
        storage.mkdir()
        cert, key = scratch / "fixture.crt", scratch / "fixture.key"
        host = "neko-relay-fixture.test"
        run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-noenc", "-days", "1", "-subj", f"/CN={host}",
            "-addext", f"subjectAltName=DNS:{host}", "-keyout", str(key), "-out", str(cert))
        settings = configure.DEFAULTS.copy()
        settings.update(PUBLIC_HOST=host, HTTPS_PORT=str(https), TURNS_PORT=str(turns),
                        TLS_CERT_FILE=str(cert), TLS_KEY_FILE=str(key), ROOM_NETWORK=network_name,
                        ROOM_NETWORK_CIDR=str(subnet), TURN_PRIVATE_IP=str(subnet.network_address + 2),
                        STORAGE_PATH=str(storage), INSTANCE_NAME=project,
                        OWNER_USERNAME="fixture-owner", OWNER_PASSWORD="fixture-$#'\\\" spaces")
        env = scratch / "fixture.env"
        env.write_text("\n".join(k + "=" + v for k, v in settings.items()) + "\n")
        run("docker", "network", "create", "--subnet", str(subnet), "--ip-range", str(list(subnet.subnets(new_prefix=25))[1]), network_name)
        compose = ["docker", "compose", "--env-file", str(state / "compose.env"), "-f", str(HERE / "compose.yaml")]
        deployed = []
        try:
            # Required input failures must happen before producing private config.
            invalid = [(k, "", k) for k in ("PUBLIC_HOST", "TLS_CERT_FILE", "TLS_KEY_FILE")]
            invalid += [("PUBLIC_HOST", "certificate-mismatch.test", "certificate"),
                        ("PUBLIC_BIND_IP", "0.0.0.0", "loopback")]
            for field, replacement, expected_error in invalid:
                broken = scratch / "broken.env"
                broken.write_text("\n".join(k + "=" + (replacement if k == field else v) for k, v in settings.items()) + "\n")
                try:
                    configure.configure(broken, scratch / "must-not-exist", fixture=True)
                except ValueError as error:
                    require(expected_error in str(error), "wrong invalid-input failure")
                else:
                    raise AssertionError(f"invalid {field} was accepted")
                require(not (scratch / "must-not-exist").exists(), "invalid inputs produced config")
            try:
                configure.configure(env, scratch / "must-not-exist", fixture=False)
            except ValueError as error:
                require("reserved" in str(error), "wrong reserved-DNS failure")
            else:
                raise AssertionError("production configuration accepted fixture DNS")
            configure.configure(env, state, fixture=True)
            saved = (state / "turn-credentials.json").read_bytes()
            configure.configure(env, state, fixture=True)
            require(saved == (state / "turn-credentials.json").read_bytes(), "configure silently rotated TURN credentials")
            require(all(p.stat().st_mode & 0o077 == 0 for p in state.iterdir()), "generated secret files are not private")
            rendered = json.loads(run(*compose, "config", "--format", "json"))
            services = rendered["services"]
            published = [(name, p) for name, service in services.items() for p in service.get("ports", [])]
            require(len(published) == 2 and all(name == "gateway" and p["protocol"] == "tcp" and p["host_ip"] == "127.0.0.1" for name, p in published), "something besides the two fixed TCP gateway listeners is host-published")
            require({p["target"] for _, p in published} == {443, 5349}, "wrong gateway listeners")
            require(all("-" not in str(p["published"]) for _, p in published), "a public port range was introduced")
            require(not any(s.get("network_mode") == "host" or s.get("privileged") for s in services.values()), "isolation bypassed")
            manager = services["neko-rooms"]["environment"]
            require(manager["NEKO_ROOMS_NETWORK_MODE"] == "relay", "manager relay mode missing")
            # Compose config escapes '$' as '$$' for round-trip serialization.
            # The HTTP fixture also verifies the actual container value by hash.
            require(manager["NEKO_ROOMS_ADMIN_PASSWORD"].replace("$$", "$") == settings["OWNER_PASSWORD"], "rendered owner password changed")
            require(manager["NEKO_ROOMS_AGENT_PUBLIC_URL"] == f"https://{host}:{https}/rooms/", "wrong Agent public prefix")
            require(json.loads(manager["NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND"])[0]["urls"] == [f"turns:{host}:{turns}?transport=tcp"], "wrong native TURN URI")
            print("PASS config: missing/mismatched cert/domain rejected; fixture WAN bind and production test-DNS rejected; private files; stable TURN secret; raw owner password; exactly two TCP gateway publications and no public relay range")
            override = scratch / "fixture.yaml"
            override.write_text(f"""services:
  neko-rooms:
    image: {PYTHON_IMAGE}
    entrypoint: [python3, /fixture/validate.py, --http-fixture]
    command: []
    volumes: !override
      - {HERE}:/fixture:ro
    healthcheck:
      test: [CMD, python3, -c, "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/')"]
      interval: 1s
      timeout: 3s
      retries: 20
  turn:
    healthcheck:
      test: [CMD, bash, -c, 'exec 3<>/dev/tcp/{subnet.network_address + 2}/3478']
      interval: 1s
      timeout: 3s
      retries: 20
  gateway:
    depends_on:
      neko-rooms: {{condition: service_healthy}}
      turn: {{condition: service_healthy}}
    healthcheck:
      test: [CMD, wget, --no-check-certificate, -q, -O, /dev/null, '--header=Host: {host}:{https}', 'https://127.0.0.1/manage/']
      interval: 1s
      timeout: 3s
      retries: 20
""")
            deployed = compose + ["-f", str(override)]
            run(*deployed, "run", "--rm", "--no-deps", "--pull", "never", "gateway", "-t")
            print("PASS mounted Nginx config: nginx -t (HTTP and stream TLS modules)")
            run(*deployed, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "40")
            context = ssl.create_default_context(cafile=str(cert))
            test_http(context, host, https, settings["OWNER_PASSWORD"])
            test_turn(context, host, turns, state, subnet)
            ids = run(*deployed, "ps", "-q").split()
            containers = json.loads(run("docker", "inspect", *ids))
            bindings = [binding for c in containers for values in (c["HostConfig"].get("PortBindings") or {}).values() for binding in values]
            require(len(bindings) == 2 and all(b["HostIp"] == "127.0.0.1" for b in bindings), "running containers published unexpected ports")
            print("PASS runtime isolation: only two loopback TCP mappings; TURN relay ports not host-published")
        except Exception:
            if deployed:
                # Preserve direct failure evidence before removing the fixtures.
                print(run(*deployed, "logs", "--no-color", "--tail", "40"), file=sys.stderr)
            raise
        finally:
            if deployed:
                run(*deployed, "down", "--remove-orphans")
            run("docker", "network", "rm", network_name)
    print("Local fixtures removed. This test proves gateway/TURN plumbing, not Neko video or a public deployment.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--http-fixture", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.http_fixture:
        fixture_server()
    else:
        try:
            validate()
        except (AssertionError, RuntimeError, ValueError, OSError, subprocess.SubprocessError) as error:
            print(f"FAIL: {error}", file=sys.stderr)
            sys.exit(1)
