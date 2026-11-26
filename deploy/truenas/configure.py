#!/usr/bin/env python3
"""Render the root TN Compose source; never start services or modify room data."""
import argparse
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import socket
import ssl
import stat
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
COMPOSE_TEMPLATE = ROOT.parent / "truenas-apps" / "neko.compose.yaml"
APP_DIR = Path("/mnt/ylangsPool/appdata/neko")
NETWORK = "neko-rooms-net"
PROJECT = "ix-neko"
MARKER = "truenas-neko-native-relay-v1\n"
PRIVATE_RANGES = tuple(ipaddress.ip_network(c) for c in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
DEFAULTS = {
    "APP_HOST": "", "TURN_HOST": "", "PUBLIC_PORT": "28322", "PUBLIC_BIND_IP": "192.168.202.11",
    "ROOM_NETWORK_CIDR": "", "TURN_PRIVATE_IP": "auto", "TLS_CERT_FILE": "", "TLS_KEY_FILE": "",
    "MANAGER_IMAGE": "docker.io/ylangylang/neko-rooms:single-port",
    "ROOM_IMAGE": "docker.io/ylangylang/neko-chromium-cdp:single-port", "RELEASE_PULL_POLICY": "always",
}
COMPOSE_KEYS = ("NEKO_APP_DIR", "RELAY_STATE_DIR", "PUBLIC_PORT", "PUBLIC_BIND_IP", "PUBLIC_ORIGIN",
                "TURN_PRIVATE_IP", "TLS_CERT_FILE", "TLS_KEY_FILE", "MANAGER_IMAGE", "ROOM_IMAGE", "RELEASE_PULL_POLICY")


def fail(message):
    raise ValueError(message)


def private_file(path, description):
    if path.is_symlink() or not path.is_file():
        fail(f"{description} must be a regular file, not a symlink: {path}")
    if path.stat().st_mode & (stat.S_IRWXG | stat.S_IRWXO):
        fail(f"{description} must be private (chmod 600): {path}")


def private_write(directory, name, content):
    target = directory / name
    if target.is_symlink():
        fail(f"refusing to replace symlink {target}")
    fd, temporary = tempfile.mkstemp(prefix=".native-relay-", dir=directory)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(content)
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def absolute_path(value, description):
    if any(c in value for c in ("\x00", "\r", "\n")):
        fail(f"{description} must be a single-line path")
    path = Path(value).expanduser()
    if not path.is_absolute() or path.is_symlink():
        fail(f"{description} must be an absolute non-symlink host path")
    return path.resolve()


def read_settings(path):
    private_file(path, "settings")
    values, seen = dict(DEFAULTS), set()
    for number, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or key not in DEFAULTS or key in seen or "\x00" in value:
            fail(f"unknown, duplicate or invalid setting on line {number}")
        seen.add(key)
        values[key] = value
    values["TURN_HOST"] = values["TURN_HOST"] or values["APP_HOST"]
    return values


def ip_literal(value):
    try:
        return ipaddress.ip_address(value)
    except ValueError:
        return None


def hostname(value):
    host = value.lower()
    address = ip_literal(host)
    if address is not None:
        return str(address)
    if len(host) > 253 or "." not in host or any(
        not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in host.split(".")
    ):
        fail("APP_HOST and TURN_HOST must be DNS names or IP addresses without scheme/path/port")
    return host


def url_host(host):
    return "[" + host + "]" if ":" in host else host


def topology(values, dev_ca, public):
    for key in ("APP_HOST", "TURN_HOST"):
        values[key] = hostname(values[key])
    bind = ipaddress.IPv4Address(values["PUBLIC_BIND_IP"])
    if not bind.is_loopback and not public:
        fail("non-loopback binding requires explicit --public")
    if not bind.is_loopback and any(values[k].endswith(".localhost") for k in ("APP_HOST", "TURN_HOST")):
        fail("LAN access needs actual client-resolvable DNS, not .localhost")
    if not re.fullmatch(r"[0-9]+", values["PUBLIC_PORT"]) or not 1 <= int(values["PUBLIC_PORT"]) <= 65535:
        fail("PUBLIC_PORT must be from 1 to 65535")
    values["PUBLIC_PORT"] = str(int(values["PUBLIC_PORT"]))
    network = ipaddress.ip_network(values["ROOM_NETWORK_CIDR"], strict=True)
    if network.version != 4 or not any(network.subnet_of(p) for p in PRIVATE_RANGES):
        fail("ROOM_NETWORK_CIDR must be the existing RFC1918 IPv4 bridge subnet")
    turn_ip = None if values["TURN_PRIVATE_IP"] in ("", "auto") else ipaddress.IPv4Address(values["TURN_PRIVATE_IP"])
    if turn_ip is not None and (turn_ip not in network or turn_ip in (network.network_address, network.broadcast_address)):
        fail("TURN_PRIVATE_IP must be auto or a usable address in ROOM_NETWORK_CIDR")
    values["TURN_PRIVATE_IP"] = str(turn_ip) if turn_ip is not None else ""
    values["TURN_BIND_IP"] = str(turn_ip) if turn_ip is not None else "0.0.0.0"
    for key in ("MANAGER_IMAGE", "ROOM_IMAGE"):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_./:@-]*", values[key]):
            fail(f"{key} must be an explicit image channel/tag/digest")
    if values["RELEASE_PULL_POLICY"] not in ("always", "missing", "never"):
        fail("RELEASE_PULL_POLICY must be always, missing or never")
    if dev_ca and (values["TLS_CERT_FILE"] or values["TLS_KEY_FILE"]):
        fail("--dev-ca cannot replace supplied TLS paths")
    authority = url_host(values["APP_HOST"]) + (":" + values["PUBLIC_PORT"] if values["PUBLIC_PORT"] != "443" else "")
    values.update(PUBLIC_AUTHORITY=authority, PUBLIC_ORIGIN="https://" + authority,
                  HTTP_AUTHORITY=url_host(values["APP_HOST"]) + (":" + values["PUBLIC_PORT"] if values["PUBLIC_PORT"] != "80" else ""),
                  ROOM_PEER_RANGE=f"{network.network_address + 1}-{network.broadcast_address - 1}",
                  TURN_RELAY_MIN="49160", TURN_RELAY_MAX="49259",
                  WEB_SNI_ROUTE=(values["APP_HOST"] + " 127.0.0.1:8443;" if values["APP_HOST"] != values["TURN_HOST"] else ""),
                  DEV_CA_LOCATION=("location = /ca.crt { alias /run/relay/public/ca.crt; "
                                   "default_type application/x-x509-ca-cert; }" if dev_ca else ""))
    return network, turn_ip


def docker(*args):
    return subprocess.run(["docker", "--host", "unix:///var/run/docker.sock", *args], capture_output=True, text=True)


def check_network(network, turn_ip):
    result = docker("network", "inspect", NETWORK)
    if result.returncode:
        fail("existing neko-rooms-net is unreadable/missing; no bridge is created automatically")
    actual = json.loads(result.stdout)[0]
    if actual["Driver"] != "bridge" or str(network) not in [x.get("Subnet") for x in actual["IPAM"].get("Config", [])]:
        fail("ROOM_NETWORK_CIDR must match the existing neko-rooms-net bridge")
    # An inspect-reported subnet may be Docker-allocated, not user-configured;
    # such bridges reject static endpoint IPs. Auto mode needs no static claim.
    if turn_ip is None:
        return
    for config in actual["IPAM"].get("Config", []):
        if str(turn_ip) in [config.get("Gateway"), *config.get("AuxiliaryAddresses", {}).values()]:
            fail("TURN_PRIVATE_IP is a gateway or reserved address")
    for container_id, endpoint in actual.get("Containers", {}).items():
        if endpoint.get("IPv4Address", "").split("/")[0] == str(turn_ip):
            result = docker("inspect", container_id)
            labels = (json.loads(result.stdout)[0]["Config"].get("Labels") or {}) if not result.returncode else {}
            if labels.get("com.docker.compose.project") != PROJECT or labels.get("com.docker.compose.service") != "turn":
                fail("TURN_PRIVATE_IP is occupied by a different container")


def openssl(*args):
    result = subprocess.run(["openssl", *args], capture_output=True, text=True)
    if result.returncode:
        fail("OpenSSL validation failed; check certificate/key, DNS/IP SANs and expiry")


def check_certificate(cert, key, values):
    if not cert.is_file():
        fail("TLS_CERT_FILE is missing")
    private_file(key, "TLS private key")
    ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(cert, key, password="")
    for host in dict.fromkeys((values["APP_HOST"], values["TURN_HOST"])):
        openssl("x509", "-in", str(cert), "-noout", "-checkip" if ip_literal(host) is not None else "-checkhost", host)
    openssl("x509", "-in", str(cert), "-noout", "-checkend", "0")


def development_cert(state, values, renew=False):
    # Adapted from the accepted single-port-relay/demo.py: existing CA/keys stay.
    directory = state / "tls"
    if directory.is_symlink():
        fail("refusing a symlink TLS directory")
    directory.mkdir(mode=0o700, exist_ok=True)
    names = ("ca.crt", "ca.key", "server.crt", "server.key")
    present = [(directory / name).exists() for name in names]
    if any(present) and not all(present):
        fail("incomplete existing CA; refusing to replace trust material")
    existing = all(present)
    if existing:
        for name in names:
            private_file(directory / name, "development trust material")
    if not existing or renew:
        with tempfile.TemporaryDirectory(prefix=".cert-", dir=directory) as temporary:
            temp = Path(temporary)
            ca = directory if existing else temp
            if not existing:
                openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3650",
                        "-subj", "/CN=Neko TrueNAS private CA", "-addext", "basicConstraints=critical,CA:TRUE",
                        "-addext", "keyUsage=critical,keyCertSign,cRLSign",
                        "-keyout", str(temp / "ca.key"), "-out", str(temp / "ca.crt"))
            key = directory / "server.key" if existing else temp / "server.key"
            key_args = ["-key", str(key)] if existing else ["-newkey", "rsa:2048", "-nodes", "-keyout", str(key)]
            openssl("req", "-new", *key_args, "-subj", "/CN=" + values["APP_HOST"], "-out", str(temp / "server.csr"))
            (temp / "extensions").write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
                                            "extendedKeyUsage=serverAuth\nsubjectAltName=" + ",".join(
                                                ("IP:" if ip_literal(h) is not None else "DNS:") + h
                                                for h in dict.fromkeys((values["APP_HOST"], values["TURN_HOST"]))) + "\n")
            openssl("x509", "-req", "-in", str(temp / "server.csr"), "-CA", str(ca / "ca.crt"),
                    "-CAkey", str(ca / "ca.key"), "-set_serial", str(secrets.randbits(159) or 1), "-days", "30",
                    "-extfile", str(temp / "extensions"), "-out", str(temp / "server.crt"))
            check_certificate(temp / "server.crt", key, values)
            openssl("verify", "-CAfile", str(ca / "ca.crt"), str(temp / "server.crt"))
            for name in (("server.crt",) if existing else names):
                private_write(directory, name, (temp / name).read_text())
    openssl("verify", "-CAfile", str(directory / "ca.crt"), str(directory / "server.crt"))
    return directory / "server.crt", directory / "server.key"


def turn_credentials(state, existing):
    path = state / "turn-credentials.json"
    if path.exists():
        private_file(path, "saved TURN credentials")
        saved = json.loads(path.read_text())
    elif existing:
        fail("owned state is missing saved TURN credentials; refusing implicit rotation")
    else:
        saved = {"username": "relay-" + secrets.token_hex(8), "password": secrets.token_hex(32)}
        private_write(state, path.name, json.dumps(saved, indent=2) + "\n")
    if (set(saved) != {"username", "password"} or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", saved["username"])
            or not re.fullmatch(r"[A-Za-z0-9_-]{32,256}", saved["password"])):
        fail("invalid saved TURN credentials; refusing replacement")
    return saved


def render_source(source, values):
    # Only the root source's complete scalar fields are parameterized. JSON
    # string quoting is valid YAML and preserves paths containing #, quotes or
    # dollars. Leave original $$ Nginx variables untouched for Compose itself.
    # Do NOT serialize `compose config`: 2.32.3 expands secret env_file values
    # into that output and has no --no-env-resolution option.
    lines = []
    for line in source.splitlines():
        if "${" not in line:
            lines.append(line)
            continue
        field = re.fullmatch(r'(\s*(?:-\s*)?[A-Za-z_][A-Za-z0-9_-]*:\s*)(.+)', line)
        sequence = re.fullmatch(r'(\s*-\s+)(".+")', line) if field is None else None
        match = field or sequence
        if match is None:
            fail("root source interpolation must occupy a scalar field or quoted list item")
        prefix, scalar = match.groups()
        if scalar.startswith('"'):
            scalar = json.loads(scalar)
        scalar = re.sub(r"\$\{([A-Z_]+)(?::[-?][^}]*)?\}", lambda m: values[m[1]], scalar)
        if "${" in scalar:
            fail("unresolved root source parameter")
        lines.append(prefix + json.dumps(scalar.replace("$", "$$")))
    return "\n".join(lines) + "\n"


def render_compose(state, values, compose_template=COMPOSE_TEMPLATE):
    rendered = render_source(Path(compose_template).read_text(), values)
    env = {k: v for k, v in os.environ.items() if k not in COMPOSE_KEYS and not k.startswith("COMPOSE_")}
    check = subprocess.run(["docker", "compose", "--project-name", PROJECT, "--env-file", "/dev/null",
                            "-f", "-", "config", "--quiet"], input=rendered, env=env, capture_output=True, text=True)
    if check.returncode:
        fail("rendered Compose validation failed; check existing NAS env files and Compose 2.32.3+")
    private_write(state, "neko.compose.yaml", rendered)


def configure(settings, app_dir=APP_DIR, dev_ca=False, public=False, renew=False, compose_template=COMPOSE_TEMPLATE):
    if renew and not dev_ca:
        fail("--renew-server-cert requires --dev-ca; supplied TLS files are never modified")
    compose_template = Path(compose_template).expanduser().resolve()
    if not compose_template.is_file():
        fail(f"Compose manifest not found: {compose_template}; pass --compose-template /path/to/truenas-apps/neko.compose.yaml")
    values = read_settings(settings)
    network, turn_ip = topology(values, dev_ca, public)
    app_dir = absolute_path(str(app_dir), "app directory")
    if app_dir.is_relative_to(ROOT):
        fail("NAS private app state must be outside the source checkout")
    # Existing files/data are preconditions, never new defaults or migration targets.
    for name in ("neko.env", "rooms.env"):
        private_file(app_dir / name, "existing permanent authentication")
    for name in ("rooms-data", "profile", "downloads"):
        if not (app_dir / name).is_dir():
            fail(f"existing {name}/ is missing; no data directory is created or moved")
    for name in ("cdp.htpasswd", "config/chromium.conf", "config/chromium-policies.json"):
        if not (app_dir / name).is_file():
            fail(f"existing legacy file is missing: {name}")
    check_network(network, turn_ip)
    if not dev_ca:
        if not values["TLS_CERT_FILE"] or not values["TLS_KEY_FILE"]:
            fail("supply existing TLS_CERT_FILE/TLS_KEY_FILE, or explicitly select --dev-ca")
        cert = absolute_path(values["TLS_CERT_FILE"], "TLS_CERT_FILE")
        key = absolute_path(values["TLS_KEY_FILE"], "TLS_KEY_FILE")
        check_certificate(cert, key, values)
    state = app_dir / "native-relay"
    marker = state / ".native-relay"
    if state.is_symlink() or (state.exists() and not state.is_dir()):
        fail("native-relay must be a non-symlink directory")
    existing = marker.exists()
    if state.exists() and any(state.iterdir()) and not existing:
        fail("native-relay contains unowned files; refusing to overwrite them")
    if existing:
        private_file(marker, "relay ownership marker")
        if marker.read_text() != MARKER:
            fail("relay ownership marker does not match this deployment")
    os.umask(0o077)
    state.mkdir(mode=0o700, exist_ok=True)
    state.chmod(0o700)
    saved = turn_credentials(state, existing)
    private_write(state, marker.name, MARKER)
    if dev_ca:
        cert, key = development_cert(state, values, renew)
        check_certificate(cert, key, values)
    public_dir = state / "public"
    if public_dir.is_symlink():
        fail("refusing a symlink public-certificate directory")
    public_dir.mkdir(mode=0o755, exist_ok=True)
    public_dir.chmod(0o755)
    if dev_ca:
        private_write(public_dir, "ca.crt", (state / "tls/ca.crt").read_text())
        (public_dir / "ca.crt").chmod(0o644)
    values.update(NEKO_APP_DIR=str(app_dir), RELAY_STATE_DIR=str(state), TLS_CERT_FILE=str(cert), TLS_KEY_FILE=str(key),
                  TURN_USERNAME=saved["username"], TURN_PASSWORD=saved["password"])
    ice = json.dumps([{"urls": [f"turns:{url_host(values['TURN_HOST'])}:{values['PUBLIC_PORT']}?transport=tcp"],
                       "username": saved["username"], "credential": saved["password"]}], separators=(",", ":"))
    private_write(state, "relay.env", "NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND=" + ice + "\n")
    for name in ("nginx.conf", "turnserver.conf", "haproxy.cfg"):
        text = (HERE / (name + ".template")).read_text()
        private_write(state, name, re.sub(r"@([A-Z_]+)@", lambda m: values[m[1]], text))
    (state / "haproxy.cfg").chmod(0o644)  # HAProxy is unprivileged; this file contains no secrets.
    render_compose(state, values, compose_template)
    return state, values["PUBLIC_ORIGIN"]


def check_endpoint(settings, app_dir=APP_DIR, private_ca=False):
    # Run AFTER deployment from the intended client's DNS/routing environment.
    # DNS may name another legitimate NAS interface or authorized WAN ingress;
    # equality with PUBLIC_BIND_IP is neither required nor proof of host identity.
    values = read_settings(settings)
    hosts = tuple(dict.fromkeys(hostname(values[k]) for k in ("APP_HOST", "TURN_HOST")))
    if not values["PUBLIC_PORT"].isdigit() or not 1 <= int(values["PUBLIC_PORT"]) <= 65535:
        fail("PUBLIC_PORT must be from 1 to 65535")
    port = int(values["PUBLIC_PORT"])
    ca = absolute_path(str(app_dir), "app directory") / "native-relay/public/ca.crt" if private_ca else None
    context = ssl.create_default_context(cafile=str(ca) if ca else None)
    observations = []
    for host in hosts:
        addresses = sorted({entry[4][0] for entry in socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)})
        with socket.create_connection((host, port), timeout=10) as connection:
            with context.wrap_socket(connection, server_hostname=host):
                pass  # Certificate SAN/trust/expiry checked; no TURN allocation is claimed.
        observations.append(f"{host}:{port} DNS={','.join(addresses)} TCP/TLS verified")
    web = http.client.HTTPSConnection(values["APP_HOST"], port, timeout=10, context=context)
    try:
        web.request("GET", "/manage/auth/login")
        response = web.getresponse()
        if response.status != 200 or response.getheader("WWW-Authenticate"):
            fail("Owner form endpoint did not return HTTP 200 without a Basic challenge")
    finally:
        web.close()
    return observations + ["Owner form HTTP 200; native room media and Agent CDP still require room-level checks."]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--settings", required=True, type=Path, help="private literal KEY=value settings; not shell/dotenv")
    parser.add_argument("--app-dir", type=Path, default=APP_DIR)
    parser.add_argument("--compose-template", type=Path, default=COMPOSE_TEMPLATE,
                        help="canonical manifest in the separate truenas-apps checkout (default: sibling checkout)")
    parser.add_argument("--public", action="store_true", help="explicitly permit the NAS/non-loopback bind")
    parser.add_argument("--dev-ca", action="store_true", help="generate/reuse a private CA, requiring explicit client trust")
    parser.add_argument("--renew-server-cert", action="store_true", help="replace ONLY the generated leaf, retaining CA/keys/TURN")
    parser.add_argument("--check-endpoint", action="store_true", help="post-deploy DNS/TCP/TLS/Owner-form probe; no files/services changed")
    args = parser.parse_args()
    try:
        if args.check_endpoint:
            if args.renew_server_cert:
                fail("endpoint checks cannot renew certificates")
            print("\n".join(check_endpoint(args.settings, args.app_dir, args.dev_ca)))
            return 0
        state, origin = configure(args.settings, args.app_dir, args.dev_ca, args.public, args.renew_server_cert,
                                  args.compose_template)
        print(f"Paste-ready TN YAML: {state}/neko.compose.yaml\nOwner: {origin}/manage/\n"
              "Existing Owner/viewer auth and room data were not changed. No services were started.")
        if args.dev_ca:
            print(f"Explicit client CA trust required: {state}/public/ca.crt (certificate only; 30-day leaf)")
        return 0
    except (ValueError, KeyError, TypeError, OSError, ssl.SSLError, subprocess.SubprocessError) as error:
        print(f"TN native relay refused: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
