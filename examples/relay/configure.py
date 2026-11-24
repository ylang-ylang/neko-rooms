#!/usr/bin/env python3
"""Render this colocated example's private config; never start/recreate services."""
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
PRIVATE_RANGES = [ipaddress.ip_network(c) for c in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")]
DEFAULTS = {
    "HTTPS_PORT": "443", "TURNS_PORT": "5349", "PUBLIC_BIND_IP": "127.0.0.1",
    "TURN_RELAY_MIN": "49160", "TURN_RELAY_MAX": "49259", "INSTANCE_NAME": "neko-rooms",
    "DOCKER_SOCKET": "/var/run/docker.sock", "MANAGER_IMAGE": "truenas-neko-rooms-agent:1.6.5-3",
    "ROOM_IMAGE": "truenas-neko-chromium-cdp:3.2.0-1", "TURN_USERNAME": "", "TURN_PASSWORD": "",
}
REQUIRED = ("PUBLIC_HOST", "TLS_CERT_FILE", "TLS_KEY_FILE", "ROOM_NETWORK", "ROOM_NETWORK_CIDR",
            "TURN_PRIVATE_IP", "STORAGE_PATH", "OWNER_USERNAME", "OWNER_PASSWORD")


def fail(message):
    raise ValueError(message)


def read_settings(path):
    # A deliberately literal subset, not executable shell or interpolated dotenv.
    # In particular, an owner's '$', '#', '\\' or quotes must survive unchanged.
    values = dict(DEFAULTS)
    seen = set()
    for number, line in enumerate(path.read_text().splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        key, value = key.strip(), value.strip()
        if not separator or not re.fullmatch(r"[A-Z][A-Z0-9_]*", key):
            fail(f"invalid KEY=value on line {number}")
        if key in seen or key not in {*DEFAULTS, *REQUIRED}:
            fail(f"unknown or duplicate setting {key} on line {number}")
        seen.add(key)
        if len(value) >= 2 and value[0] in "\"'" and value[-1] == value[0]:
            value = value[1:-1]
        if "\x00" in value or "\r" in value or "\n" in value:
            fail(f"{key} must be a single literal line")
        values[key] = value
    for key in REQUIRED:
        if not values.get(key):
            fail(f"{key} is required; no example domain/certificate/owner fallback is provided")
    return values


def check_private_file(path, description):
    if not path.is_file():
        fail(f"{description} is not a file: {path}")
    if path.stat().st_mode & (stat.S_IRWXG | stat.S_IRWXO):
        fail(f"{description} must be private (chmod 600): {path}")


def absolute_path(value, description):
    path = Path(value).expanduser()
    if not path.is_absolute():
        fail(f"{description} must be an absolute host path")
    return path.resolve()


def number(values, key):
    if not values[key].isdigit() or not 1 <= int(values[key]) <= 65535:
        fail(f"{key} must be a port from 1 to 65535")
    values[key] = str(int(values[key]))
    return int(values[key])


def check_network(values, network, turn_ip):
    result = subprocess.run(["docker", "network", "inspect", values["ROOM_NETWORK"]],
                            capture_output=True, text=True, check=False)
    if result.returncode:
        fail("ROOM_NETWORK does not exist on this Docker daemon; inspect/create the intended bridge first")
    actual = json.loads(result.stdout)[0]
    if actual["Driver"] != "bridge":
        fail("this colocated example requires a Docker bridge, not host/macvlan/overlay networking")
    subnets = [x.get("Subnet") for x in actual["IPAM"].get("Config", [])]
    if str(network) not in subnets:
        fail("ROOM_NETWORK_CIDR does not match docker network inspect")
    for config in actual["IPAM"].get("Config", []):
        reserved = [config.get("Gateway"), *config.get("AuxiliaryAddresses", {}).values()]
        if str(turn_ip) in reserved:
            fail("TURN_PRIVATE_IP is a network gateway or reserved auxiliary address")
    for endpoint in actual.get("Containers", {}).values():
        if endpoint.get("IPv4Address", "").split("/")[0] == str(turn_ip):
            if endpoint["Name"] != values["COMPOSE_PROJECT_NAME"] + "-turn-1":
                fail("TURN_PRIVATE_IP is already occupied by a different container")


def private_write(directory, name, content):
    target = directory / name
    if target.is_symlink():
        fail(f"refusing to replace symlink {target}")
    fd, temporary = tempfile.mkstemp(prefix=".relay-", dir=directory)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(content)
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def configure(env_file, output, fixture=False):
    check_private_file(env_file, "operator settings")
    values = read_settings(env_file)
    host = values["PUBLIC_HOST"].lower()
    if len(host) > 253 or not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", host):
        fail("PUBLIC_HOST must be a DNS hostname, without scheme, path or port")
    if any(not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in host.split(".")):
        fail("PUBLIC_HOST contains an invalid DNS label")
    if "." not in host:
        fail("PUBLIC_HOST must be a fully qualified DNS hostname")
    try:
        ipaddress.ip_address(host)
    except ValueError:
        pass
    else:
        fail("PUBLIC_HOST must be a DNS hostname, not an IP address")
    reserved = host.endswith((".test", ".invalid", ".localhost", ".example")) or any(
        host == d or host.endswith("." + d) for d in ("example.com", "example.net", "example.org"))
    bind_ip = ipaddress.IPv4Address(values["PUBLIC_BIND_IP"])
    if fixture and not bind_ip.is_loopback:
        fail("fixture mode can bind only to loopback")
    if reserved and not fixture:
        fail("PUBLIC_HOST is reserved documentation/test DNS; supply the user's real domain")
    values["PUBLIC_HOST"] = host
    https = number(values, "HTTPS_PORT")
    turns = number(values, "TURNS_PORT")
    if https == turns:
        fail("HTTPS_PORT and TURNS_PORT must be different TCP listeners")
    low, high = number(values, "TURN_RELAY_MIN"), number(values, "TURN_RELAY_MAX")
    if low > high or low < 1024:
        fail("TURN_RELAY_MIN/MAX must be an ordered, unprivileged private UDP range")
    network = ipaddress.ip_network(values["ROOM_NETWORK_CIDR"], strict=True)
    if network.version != 4 or not any(network.subnet_of(p) for p in PRIVATE_RANGES):
        fail("ROOM_NETWORK_CIDR must be an RFC1918 IPv4 subnet")
    turn_ip = ipaddress.IPv4Address(values["TURN_PRIVATE_IP"])
    if turn_ip not in network or turn_ip in (network.network_address, network.broadcast_address):
        fail("TURN_PRIVATE_IP must be a usable address inside ROOM_NETWORK_CIDR")
    for key in ("ROOM_NETWORK", "INSTANCE_NAME"):
        if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]*", values[key]):
            fail(f"{key} is not a Docker-compatible name")
    values["COMPOSE_PROJECT_NAME"] = values["INSTANCE_NAME"].lower() + "-relay"
    cert = absolute_path(values["TLS_CERT_FILE"], "TLS_CERT_FILE")
    key = absolute_path(values["TLS_KEY_FILE"], "TLS_KEY_FILE")
    if not cert.is_file():
        fail("TLS_CERT_FILE is missing; supply a PEM full chain matching PUBLIC_HOST")
    check_private_file(key, "TLS private key")
    ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(cert, key, password="")
    for args in (["-checkhost", host], ["-checkend", "0"]):
        result = subprocess.run(["openssl", "x509", "-in", str(cert), "-noout", *args],
                                capture_output=True, check=False)
        if result.returncode:
            fail("TLS certificate is expired or does not cover PUBLIC_HOST")
    storage = absolute_path(values["STORAGE_PATH"], "STORAGE_PATH")
    if not storage.is_dir():
        fail("STORAGE_PATH must be the existing host directory; it will not be created or migrated")
    socket = absolute_path(values["DOCKER_SOCKET"], "DOCKER_SOCKET")
    if not socket.is_socket():
        fail("DOCKER_SOCKET must be the actual local Docker Unix socket")
    output = output.expanduser().resolve()
    if output.is_relative_to(ROOT):
        fail("private output must be outside the source checkout")
    check_network(values, network, turn_ip)
    # All required topology/cert inputs have now passed, before generating secrets.
    os.umask(0o077)
    output.mkdir(parents=True, mode=0o700, exist_ok=True)
    output.chmod(0o700)
    credential_file = output / "turn-credentials.json"
    username, password = values["TURN_USERNAME"], values["TURN_PASSWORD"]
    if bool(username) != bool(password):
        fail("set both TURN_USERNAME and TURN_PASSWORD, or leave both blank")
    if not username:
        if credential_file.exists():
            check_private_file(credential_file, "saved TURN credentials")
            saved = json.loads(credential_file.read_text())
            username, password = saved["username"], saved["password"]
        else:
            username, password = "relay-" + secrets.token_hex(8), secrets.token_hex(32)
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", username) or not re.fullmatch(r"[A-Za-z0-9_-]{32,256}", password):
        fail("TURN credentials require an alphanumeric/_/- username and 32+ character password")
    values.update(TURN_USERNAME=username, TURN_PASSWORD=password, RELAY_STATE_DIR=str(output),
                  TLS_CERT_FILE=str(cert), TLS_KEY_FILE=str(key), STORAGE_PATH=str(storage), DOCKER_SOCKET=str(socket),
                  PUBLIC_AUTHORITY=host if https == 443 else f"{host}:{https}",
                  ROOM_PEER_RANGE=f"{network.network_address + 1}-{network.broadcast_address - 1}")
    values["PUBLIC_ORIGIN"] = "https://" + values["PUBLIC_AUTHORITY"]
    ice = json.dumps([{"urls": [f"turns:{host}:{turns}?transport=tcp"],
                       "username": username, "credential": password}], separators=(",", ":"))
    private_write(output, "turn-credentials.json", json.dumps({"username": username, "password": password}) + "\n")
    private_write(output, "manager.env", "\n".join([
        "NEKO_ROOMS_ADMIN_USERNAME=" + values["OWNER_USERNAME"],
        "NEKO_ROOMS_ADMIN_PASSWORD=" + values["OWNER_PASSWORD"],
        "NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND=" + ice, ""]))
    for name in ("nginx.conf", "turnserver.conf"):
        text = (HERE / (name + ".template")).read_text()
        text = re.sub(r"@([A-Z_]+)@", lambda m: values[m[1]], text)
        private_write(output, name, text)
    public_keys = ("COMPOSE_PROJECT_NAME", "RELAY_STATE_DIR", "PUBLIC_HOST", "PUBLIC_ORIGIN", "PUBLIC_BIND_IP",
                   "HTTPS_PORT", "TURNS_PORT", "TLS_CERT_FILE", "TLS_KEY_FILE", "ROOM_NETWORK", "ROOM_NETWORK_CIDR",
                   "TURN_PRIVATE_IP", "STORAGE_PATH", "DOCKER_SOCKET", "INSTANCE_NAME", "MANAGER_IMAGE", "ROOM_IMAGE")
    # Compose single quotes suppress interpolation, including '$' in host paths.
    lines = [k + "='" + values[k].replace("'", "\\'") + "'" for k in public_keys]
    private_write(output, "compose.env", "\n".join(lines) + "\n")
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env-file", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--fixture", action="store_true", help="local tests only: permit reserved DNS, require loopback binding")
    args = parser.parse_args()
    try:
        directory = configure(args.env_file, args.output_dir, args.fixture)
    except (ValueError, OSError, ssl.SSLError, subprocess.SubprocessError) as error:
        print(f"configuration refused: {error}", file=sys.stderr)
        return 1
    print(f"Private config ready: {directory}/compose.env (no services started)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
