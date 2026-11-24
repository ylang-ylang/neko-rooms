#!/usr/bin/env python3
"""Configure a fresh native Neko single-TCP-port demo; keep all state private."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import stat
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
ROOM_IMAGE = "ghcr.io/m1k1o/neko/chromium:3.2.0@sha256:270face8b8111e639744fb7bcdbbf0deed227900bb89d58b2f8adfc12af8c41e"
PRIVATE_RANGES = tuple(ipaddress.ip_network(c) for c in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
DEFAULTS = {
    "APP_HOST": "neko.localhost", "TURN_HOST": "turn.localhost", "PUBLIC_PORT": "8443",
    "PUBLIC_BIND_IP": "127.0.0.1", "MANAGER_IMAGE": "", "ROOM_IMAGE": ROOM_IMAGE,
    "ROOM_NETWORK": "", "ROOM_NETWORK_CIDR": "", "TURN_PRIVATE_IP": "",
    "INSTANCE_NAME": "neko-single-port", "DOCKER_SOCKET": "/var/run/docker.sock",
    "TURN_RELAY_MIN": "49160", "TURN_RELAY_MAX": "49259", "TLS_CERT_FILE": "", "TLS_KEY_FILE": "",
    "OWNER_USERNAME": "", "OWNER_PASSWORD": "", "TURN_USERNAME": "", "TURN_PASSWORD": "",
}
COMPOSE_KEYS = (
    "COMPOSE_PROJECT_NAME", "RELAY_STATE_DIR", "APP_HOST", "TURN_HOST", "PUBLIC_PORT", "PUBLIC_BIND_IP",
    "PUBLIC_ORIGIN", "TLS_CERT_FILE", "TLS_KEY_FILE", "MANAGER_IMAGE", "ROOM_IMAGE", "ROOM_NETWORK",
    "ROOM_NETWORK_CIDR", "TURN_PRIVATE_IP", "INSTANCE_NAME", "DOCKER_SOCKET", "STORAGE_PATH",
)
MARKER = "native-single-port-relay-v1\n"


def fail(message):
    raise ValueError(message)


def check_private_file(path, description):
    if path.is_symlink() or not path.is_file():
        fail(f"{description} must be a regular file, not a symlink: {path}")
    if path.stat().st_mode & (stat.S_IRWXG | stat.S_IRWXO):
        fail(f"{description} must be private (chmod 600): {path}")


def absolute_path(value, description):
    if any(c in value for c in ("\x00", "\r", "\n")):
        fail(f"{description} must be a single-line path")
    path = Path(value).expanduser()
    if not path.is_absolute():
        fail(f"{description} must be an absolute host path")
    return path.resolve()


def private_write(directory, name, content):
    target = directory / name
    if target.is_symlink():
        fail(f"refusing to replace symlink {target}")
    fd, temporary = tempfile.mkstemp(prefix=".single-port-", dir=directory)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(content)
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def read_settings(path):
    check_private_file(path, "settings")
    values, seen = dict(DEFAULTS), set()
    for number, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or key not in DEFAULTS or key in seen:
            fail(f"unknown, duplicate or invalid setting on line {number}")
        if any(c in value for c in ("\x00", "\r", "\n")):
            fail(f"{key} must be a single literal line")
        seen.add(key)
        values[key] = value
    for key in ("MANAGER_IMAGE", "ROOM_NETWORK", "ROOM_NETWORK_CIDR", "TURN_PRIVATE_IP"):
        if not values[key]:
            fail(f"{key} is required; no image/network is guessed")
    return values


def hostname(value):
    host = value.lower()
    if len(host) > 253 or "." not in host or any(
        not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in host.split(".")
    ):
        fail("APP_HOST and TURN_HOST must be DNS hostnames without scheme, path or port")
    try:
        ipaddress.ip_address(host)
    except ValueError:
        return host
    fail("APP_HOST and TURN_HOST must be DNS names, not IP addresses (web SNI is required)")


def reserved(host):
    return host.endswith((".test", ".invalid", ".localhost", ".example")) or any(
        host == d or host.endswith("." + d) for d in ("example.com", "example.net", "example.org")
    )


def port(values, key):
    if not re.fullmatch(r"[0-9]+", values[key]) or not 1 <= int(values[key]) <= 65535:
        fail(f"{key} must be a port from 1 to 65535")
    values[key] = str(int(values[key]))
    return int(values[key])


def validate_topology(values, dev_ca, public):
    for key in ("APP_HOST", "TURN_HOST"):
        values[key] = hostname(values[key])
    if values["APP_HOST"] == values["TURN_HOST"]:
        fail("APP_HOST and TURN_HOST must differ: explicit web SNI selects HTTPS, everything else selects TURN")
    bind = ipaddress.IPv4Address(values["PUBLIC_BIND_IP"])
    if dev_ca:
        if public or not bind.is_loopback or not all(values[k].endswith(".localhost") for k in ("APP_HOST", "TURN_HOST")):
            fail("--dev-ca requires .localhost names and loopback binding, without --public")
        if values["TLS_CERT_FILE"] or values["TLS_KEY_FILE"]:
            fail("--dev-ca cannot also use supplied TLS_CERT_FILE/TLS_KEY_FILE")
    elif any(reserved(values[k]) for k in ("APP_HOST", "TURN_HOST")):
        fail("production requires real DNS names; local .localhost setup requires --dev-ca")
    if not bind.is_loopback and not public:
        fail("non-loopback binding requires explicit --public and real DNS/certificate inputs")
    port(values, "PUBLIC_PORT")
    low, high = port(values, "TURN_RELAY_MIN"), port(values, "TURN_RELAY_MAX")
    if low < 1024 or low > high:
        fail("TURN_RELAY_MIN/MAX must be an ordered unprivileged private UDP range")
    network = ipaddress.ip_network(values["ROOM_NETWORK_CIDR"], strict=True)
    if network.version != 4 or not any(network.subnet_of(p) for p in PRIVATE_RANGES):
        fail("ROOM_NETWORK_CIDR must be an RFC1918 IPv4 subnet")
    turn_ip = ipaddress.IPv4Address(values["TURN_PRIVATE_IP"])
    if turn_ip not in network or turn_ip in (network.network_address, network.broadcast_address):
        fail("TURN_PRIVATE_IP must be a usable address in ROOM_NETWORK_CIDR")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_.-]*", values["ROOM_NETWORK"]):
        fail("ROOM_NETWORK must be a lower-case Docker-compatible name")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", values["INSTANCE_NAME"]):
        fail("INSTANCE_NAME must be a lower-case Compose-compatible name (letters, digits, _ and -)")
    for key in ("MANAGER_IMAGE", "ROOM_IMAGE"):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_./:@-]*", values[key]):
            fail(f"{key} must be a single Docker image reference")
    values["COMPOSE_PROJECT_NAME"] = values["INSTANCE_NAME"] + "-single-port"
    values["ROOM_PEER_RANGE"] = f"{network.network_address + 1}-{network.broadcast_address - 1}"
    authority = values["APP_HOST"] + (":" + values["PUBLIC_PORT"] if values["PUBLIC_PORT"] != "443" else "")
    values["PUBLIC_AUTHORITY"], values["PUBLIC_ORIGIN"] = authority, "https://" + authority
    return network, turn_ip


def check_network(values, network, turn_ip):
    result = subprocess.run(["docker", "network", "inspect", values["ROOM_NETWORK"]], capture_output=True, text=True)
    if result.returncode:
        fail("ROOM_NETWORK is missing/unreadable; explicitly create/select the intended local bridge first")
    actual = json.loads(result.stdout)[0]
    if actual["Driver"] != "bridge" or str(network) not in [x.get("Subnet") for x in actual["IPAM"].get("Config", [])]:
        fail("ROOM_NETWORK must be a Docker bridge with the exact ROOM_NETWORK_CIDR")
    for config in actual["IPAM"].get("Config", []):
        if str(turn_ip) in [config.get("Gateway"), *config.get("AuxiliaryAddresses", {}).values()]:
            fail("TURN_PRIVATE_IP is a network gateway or reserved auxiliary address")
    for endpoint in actual.get("Containers", {}).values():
        if endpoint.get("IPv4Address", "").split("/")[0] == str(turn_ip):
            if endpoint["Name"] != values["COMPOSE_PROJECT_NAME"] + "-turn-1":
                fail("TURN_PRIVATE_IP is occupied by another container")


def run_openssl(*args):
    result = subprocess.run(["openssl", *args], capture_output=True, text=True)
    if result.returncode:
        # Do not echo command output: operators may supply secret-bearing paths.
        fail("OpenSSL operation failed; check certificate, key, hostnames and validity")


def development_cert(state, values):
    directory = state / "tls"
    if directory.is_symlink():
        fail("refusing a symlink TLS directory")
    directory.mkdir(mode=0o700, exist_ok=True)
    names = ("ca.crt", "ca.key", "server.crt", "server.key")
    present = [(directory / name).exists() for name in names]
    if any(present) and not all(present):
        fail("incomplete existing development CA; refusing to replace trust material")
    if not any(present):
        with tempfile.TemporaryDirectory(prefix=".cert-", dir=directory) as temp:
            temp = Path(temp)
            run_openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3650",
                        "-subj", "/CN=Neko single-port local development CA",
                        "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign",
                        "-keyout", str(temp / "ca.key"), "-out", str(temp / "ca.crt"))
            run_openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=" + values["APP_HOST"],
                        "-keyout", str(temp / "server.key"), "-out", str(temp / "server.csr"))
            (temp / "extensions").write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
                                            "extendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + values["APP_HOST"] + ",DNS:" + values["TURN_HOST"] + "\n")
            run_openssl("x509", "-req", "-in", str(temp / "server.csr"), "-CA", str(temp / "ca.crt"),
                        "-CAkey", str(temp / "ca.key"), "-set_serial", str(secrets.randbits(159) or 1), "-days", "30",
                        "-extfile", str(temp / "extensions"), "-out", str(temp / "server.crt"))
            for name in names:
                private_write(directory, name, (temp / name).read_text())
    for name in names:
        check_private_file(directory / name, "development trust material")
    run_openssl("verify", "-CAfile", str(directory / "ca.crt"), str(directory / "server.crt"))
    return directory / "server.crt", directory / "server.key"


def check_certificate(cert, key, values):
    if not cert.is_file():
        fail("TLS_CERT_FILE is missing")
    check_private_file(key, "TLS private key")
    ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(cert, key, password="")
    for host in (values["APP_HOST"], values["TURN_HOST"]):
        run_openssl("x509", "-in", str(cert), "-noout", "-checkhost", host)
    run_openssl("x509", "-in", str(cert), "-noout", "-checkend", "0")


def credentials(state, values):
    saved_file = state / "credentials.json"
    if saved_file.exists():
        check_private_file(saved_file, "saved credentials")
        saved = json.loads(saved_file.read_text())
    else:
        saved = {
            "owner_username": values["OWNER_USERNAME"] or "owner",
            "owner_password": values["OWNER_PASSWORD"] or secrets.token_urlsafe(32),
            "room_user_password": secrets.token_urlsafe(32), "room_admin_password": secrets.token_urlsafe(32),
            "turn_username": values["TURN_USERNAME"] or "relay-" + secrets.token_hex(8),
            "turn_password": values["TURN_PASSWORD"] or secrets.token_hex(32),
        }
    for prefix in ("OWNER", "TURN"):
        username, password = values[prefix + "_USERNAME"], values[prefix + "_PASSWORD"]
        if bool(username) != bool(password):
            fail(f"set both {prefix}_USERNAME and {prefix}_PASSWORD, or leave both blank")
        if username and (username != saved[prefix.lower() + "_username"] or password != saved[prefix.lower() + "_password"]):
            fail(f"saved {prefix} credentials differ; no implicit credential rotation (use a fresh demo state)")
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", saved["turn_username"]) or not re.fullmatch(r"[A-Za-z0-9_-]{32,256}", saved["turn_password"]):
        fail("TURN credentials require an alphanumeric/_/- username and a 32+ character password")
    private_write(state, "credentials.json", json.dumps(saved, indent=2) + "\n")
    return saved


def compose_literal(value):
    # Compose dotenv single quotes inhibit $ interpolation; \' embeds a quote.
    return "'" + value.replace("'", "\\'") + "'"


def configure(settings, state, dev_ca=False, public=False):
    values = read_settings(settings)
    network, turn_ip = validate_topology(values, dev_ca, public)
    socket = absolute_path(values["DOCKER_SOCKET"], "DOCKER_SOCKET")
    if not socket.is_socket():
        fail("DOCKER_SOCKET must be the actual local Docker Unix socket")
    if state.expanduser().is_symlink():
        fail("refusing a symlink state directory")
    state = absolute_path(str(state), "state directory")
    if state.is_relative_to(ROOT):
        fail("private state must be outside the source checkout")
    check_network(values, network, turn_ip)
    if not dev_ca:
        if not values["TLS_CERT_FILE"] or not values["TLS_KEY_FILE"]:
            fail("TLS_CERT_FILE and TLS_KEY_FILE are required without --dev-ca")
        cert = absolute_path(values["TLS_CERT_FILE"], "TLS_CERT_FILE")
        key = absolute_path(values["TLS_KEY_FILE"], "TLS_KEY_FILE")
        check_certificate(cert, key, values)
    marker = state / ".single-port-relay"
    if state.exists():
        if not state.is_dir():
            fail("state directory must be a directory")
        if any(state.iterdir()) and not marker.is_file():
            fail("state directory is not owned by this example; use a fresh empty directory")
        if marker.exists():
            check_private_file(marker, "state ownership marker")
            if marker.read_text() != MARKER:
                fail("state ownership marker does not match this example")
    os.umask(0o077)
    state.mkdir(parents=True, mode=0o700, exist_ok=True)
    state.chmod(0o700)
    private_write(state, marker.name, MARKER)
    if dev_ca:
        cert, key = development_cert(state, values)
        check_certificate(cert, key, values)
    storage = state / "storage"
    if storage.is_symlink():
        fail("refusing symlink storage; this demo never reuses existing installation data")
    storage.mkdir(mode=0o700, exist_ok=True)
    saved = credentials(state, values)
    values.update(RELAY_STATE_DIR=str(state), TLS_CERT_FILE=str(cert), TLS_KEY_FILE=str(key),
                  DOCKER_SOCKET=str(socket), STORAGE_PATH=str(storage),
                  TURN_USERNAME=saved["turn_username"], TURN_PASSWORD=saved["turn_password"])
    ice = json.dumps([{"urls": [f"turns:{values['TURN_HOST']}:{values['PUBLIC_PORT']}?transport=tcp"],
                       "username": saved["turn_username"], "credential": saved["turn_password"]}], separators=(",", ":"))
    private_write(state, "manager.env", "\n".join([
        "NEKO_ROOMS_ADMIN_USERNAME=" + saved["owner_username"],
        "NEKO_ROOMS_ADMIN_PASSWORD=" + saved["owner_password"],
        "NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND=" + ice, ""]))
    for name in ("nginx.conf", "turnserver.conf"):
        text = (HERE / (name + ".template")).read_text()
        private_write(state, name, re.sub(r"@([A-Z_]+)@", lambda m: values[m[1]], text))
    private_write(state, "compose.env", "\n".join(k + "=" + compose_literal(values[k]) for k in COMPOSE_KEYS) + "\n")
    request = {
        "api_version": 3, "name": "native", "neko_image": values["ROOM_IMAGE"],
        "user_pass": saved["room_user_password"], "admin_pass": saved["room_admin_password"],
        "max_connections": 10, "screen": "1920x1080@30", "video_codec": "VP8", "video_bitrate": 3072,
        "video_max_fps": 25, "audio_codec": "OPUS", "audio_bitrate": 128, "envs": {},
        "mounts": [{"type": "private", "host_path": "/profile", "container_path": "/home/neko/profile"},
                   {"type": "private", "host_path": "/downloads", "container_path": "/home/neko/Downloads"}],
        "resources": {"shm_size": 2147483648},
    }
    private_write(state, "room-request.json", json.dumps(request, indent=2) + "\n")
    return state


def compose_command(state, arguments):
    env_file = absolute_path(str(state), "state directory") / "compose.env"
    check_private_file(env_file, "generated Compose environment")
    return ["docker", "compose", "--env-file", str(env_file), "-f", str(HERE / "compose.yaml"), *arguments]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    config = sub.add_parser("configure", help="validate/render private state; do not start containers")
    config.add_argument("--env-file", required=True, type=Path)
    config.add_argument("--state-dir", required=True, type=Path)
    config.add_argument("--dev-ca", action="store_true", help="explicit local .localhost CA/cert generation; loopback only")
    config.add_argument("--public", action="store_true", help="explicitly permit non-loopback real-DNS deployment")
    for name in ("up", "down", "status"):
        sub.add_parser(name).add_argument("--state-dir", required=True, type=Path)
    args = parser.parse_args()
    try:
        if args.command == "configure":
            state = configure(args.env_file, args.state_dir, args.dev_ca, args.public)
            print(f"Private config: {state}/compose.env\nCredentials: {state}/credentials.json (not printed; no services started)")
            if args.dev_ca:
                print(f"Explicit browser trust required: {state}/tls/ca.crt (no automatic OS/browser changes)")
            return 0
        arguments = {"up": ["up", "-d"], "down": ["down"], "status": ["ps"]}[args.command]
        return subprocess.run(compose_command(args.state_dir, arguments)).returncode
    except (ValueError, KeyError, OSError, ssl.SSLError, subprocess.SubprocessError) as error:
        print(f"single-port demo refused: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
