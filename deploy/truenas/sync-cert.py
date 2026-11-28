#!/usr/bin/env python3
"""Safely reload/restart ONLY ix-neko-gateway-1 when an external ACME cert renews."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import socket
import ssl
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import configure as cfg  # noqa: E402

DEFAULT_CONTAINER = "ix-neko-gateway-1"


def pem_leaf_sha256(pem_bytes):
    result = subprocess.run(
        ["openssl", "x509", "-outform", "DER"],
        input=pem_bytes,
        capture_output=True,
    )
    if result.returncode != 0 or not result.stdout:
        raise ValueError("unable to parse leaf certificate PEM")
    return hashlib.sha256(result.stdout).hexdigest()


def probe_live_leaf_sha256(host, port, server_name, timeout=5):
    # Only compares the served leaf DER hash against the validated host file.
    context = ssl._create_unverified_context()
    with socket.create_connection((host, int(port)), timeout=timeout) as sock:
        with context.wrap_socket(sock, server_hostname=server_name) as tls:
            der = tls.getpeercert(binary_form=True)
    if not der:
        raise RuntimeError("gateway returned no peer certificate")
    return hashlib.sha256(der).hexdigest()


def container_file_sha256(container, path):
    result = subprocess.run(
        ["docker", "--host", "unix:///var/run/docker.sock", "exec", container, "cat", path],
        capture_output=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"unable to read {path} inside {container}")
    # Hash in memory; never return or log raw private key material.
    return hashlib.sha256(result.stdout).hexdigest()


def sync_certificate(settings_path, container=DEFAULT_CONTAINER, dry_run=False, wait_seconds=10):
    values = cfg.read_settings(settings_path)
    if values["TRANSPORT"] != "tls":
        return {"status": "skipped", "reason": "not-external-tls"}
    has_cert, has_key = bool(values.get("TLS_CERT_FILE")), bool(values.get("TLS_KEY_FILE"))
    if not has_cert and not has_key:
        return {"status": "skipped", "reason": "not-external-tls"}
    if has_cert != has_key:
        raise ValueError("TLS_CERT_FILE and TLS_KEY_FILE must both be set")

    values["APP_HOST"] = cfg.hostname(values["APP_HOST"])
    values["TURN_HOST"] = cfg.hostname(values["TURN_HOST"])
    if not re.fullmatch(r"[0-9]+", values["PUBLIC_PORT"]) or not 1 <= int(values["PUBLIC_PORT"]) <= 65535:
        raise ValueError("PUBLIC_PORT must be from 1 to 65535")

    cert = cfg.absolute_path(values["TLS_CERT_FILE"], "TLS_CERT_FILE")
    key = cfg.absolute_path(values["TLS_KEY_FILE"], "TLS_KEY_FILE")
    # Fail closed before touching the running gateway if the host cert/key pair is invalid.
    cfg.check_certificate(cert, key, values)

    cert_bytes = cert.read_bytes()
    key_bytes = key.read_bytes()
    host_fp = pem_leaf_sha256(cert_bytes)
    host_cert_raw_sha = hashlib.sha256(cert_bytes).hexdigest()
    host_key_raw_sha = hashlib.sha256(key_bytes).hexdigest()

    bind_ip = "127.0.0.1" if values["PUBLIC_BIND_IP"] == "0.0.0.0" else values["PUBLIC_BIND_IP"]
    bind_port = int(values["PUBLIC_PORT"])
    sni = values["APP_HOST"]
    live_fp = probe_live_leaf_sha256(bind_ip, bind_port, sni)

    if live_fp == host_fp:
        return {"status": "unchanged", "fingerprint": host_fp}
    if dry_run:
        return {"status": "reload_needed", "live_fingerprint": live_fp, "host_fingerprint": host_fp}

    # Check BOTH cert and key mounted inside the container so an atomic rename of
    # either file takes the gateway_restart path instead of failing nginx -t.
    mounted_cert_sha = container_file_sha256(container, "/run/relay/tls.crt")
    mounted_key_sha = container_file_sha256(container, "/run/relay/tls.key")
    if mounted_cert_sha == host_cert_raw_sha and mounted_key_sha == host_key_raw_sha:
        check = cfg.docker("exec", container, "nginx", "-t")
        if check.returncode != 0:
            raise RuntimeError("nginx -t failed before reload: " + (check.stderr or "").strip()[:300])
        reload_res = cfg.docker("exec", container, "nginx", "-s", "reload")
        if reload_res.returncode != 0:
            raise RuntimeError("nginx -s reload failed: " + (reload_res.stderr or "").strip()[:300])
        action = "nginx_reload"
    else:
        # TrueNAS ACME renewal replaced a host file inode; restart ONLY the gateway
        # container so Docker re-binds the updated host paths without touching Manager or rooms.
        restart_res = cfg.docker("restart", container)
        if restart_res.returncode != 0:
            raise RuntimeError("docker restart failed: " + (restart_res.stderr or "").strip()[:300])
        action = "gateway_restart"

    deadline = time.monotonic() + max(wait_seconds, 1)
    latest_fp = live_fp
    while time.monotonic() < deadline:
        try:
            latest_fp = probe_live_leaf_sha256(bind_ip, bind_port, sni)
            if latest_fp == host_fp:
                return {
                    "status": "reloaded",
                    "action": action,
                    "previous_fingerprint": live_fp,
                    "fingerprint": host_fp,
                }
        except OSError:
            pass
        time.sleep(0.5)
    raise RuntimeError(f"gateway still serves {latest_fp[:16]} after {action}, expected {host_fp[:16]}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--settings", required=True, type=Path, help="TrueNAS native-relay.settings path")
    parser.add_argument("--container", default=DEFAULT_CONTAINER, help="gateway container name")
    parser.add_argument("--dry-run", action="store_true", help="check without reloading/restarting gateway")
    args = parser.parse_args()
    try:
        print(json.dumps(sync_certificate(args.settings, args.container, args.dry_run), indent=2))
        return 0
    except Exception as error:
        print(json.dumps({"status": "failed", "error_type": type(error).__name__, "error": str(error)[:400]}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
