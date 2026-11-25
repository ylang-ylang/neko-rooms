#!/usr/bin/env python3
"""Update only the existing TN App's Manager image; retain its current topology."""
import argparse
import copy
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

APP = "neko"
APPDATA = Path("/mnt/ylangsPool/appdata/neko")
DEFAULT_IMAGE = "docker.io/ylangylang/neko-rooms:single-port"


def update(image, standby_image=None):
    from truenas_api_client import Client

    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    backup = APPDATA / "backups" / ("manager-update-" + stamp)
    backup.mkdir(parents=True, mode=0o700)
    # Immutable digests already delivered locally need no second registry call.
    # Browser delivery is standby-only: never change a room or its allowlist here.
    for kind, reference in (("manager", image), ("browser", standby_image)):
        if not reference:
            continue
        present = subprocess.run(["docker", "image", "inspect", reference], stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL).returncode == 0 if "@sha256:" in reference else False
        if not present:
            log_path = backup / ("pull-" + kind + ".log")
            with log_path.open("w") as log:
                result = subprocess.run(["docker", "pull", reference], stdout=log, stderr=subprocess.STDOUT)
            if result.returncode:
                raise RuntimeError(kind + " pull failed; first error is in " + str(log_path))
    with Client() as client:
        original = client.call("app.config", APP)
        (backup / "app-config.json").write_text(json.dumps(original, indent=2) + "\n")
        for name in ("compose.yaml", "rooms.env", "neko.env"):
            if (APPDATA / name).is_file():
                shutil.copy2(APPDATA / name, backup / name)
                (backup / name).chmod(0o600)
        config = copy.deepcopy(original)
        manager = config["services"]["neko-rooms"]
        previous = manager["image"]
        manager["image"] = image
        # No env, allowlist, port, mount, other service, room or network changes.
        if config == original and "@sha256:" in image:
            return {"status": "unchanged", "manager_image": image,
                    "url": manager.get("environment", {}).get("NEKO_ROOMS_INSTANCE_URL"),
                    "network_mode": manager.get("environment", {}).get("NEKO_ROOMS_NETWORK_MODE", "direct")}
        try:
            client.call("app.update", APP, {"custom_compose_config_string": json.dumps(config)}, job=True)
            result = client.call("app.get_instance", APP)
        except Exception:
            # app.update can fail after writing the new definition. Restore via
            # the same official API, never through a second Compose project.
            client.call("app.update", APP, {"custom_compose_config_string": json.dumps(original)}, job=True)
            raise
    mirror = APPDATA / "compose.yaml"
    temporary = APPDATA / ".compose.yaml.manager-update"
    temporary.write_text(json.dumps(config, indent=2) + "\n")
    if mirror.exists():
        shutil.copymode(mirror, temporary)
    os.replace(temporary, mirror)
    receipt = {"status": "updated", "app_state": (result or {}).get("state"), "manager_image": image,
               "previous_manager_image": previous,
               "network_mode": manager.get("environment", {}).get("NEKO_ROOMS_NETWORK_MODE", "direct"),
               "url": manager.get("environment", {}).get("NEKO_ROOMS_INSTANCE_URL"),
               "owner_credentials": str(APPDATA / "rooms.env"), "rollback_config": str(backup / "app-config.json")}
    (backup / "result.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manager-image", default=DEFAULT_IMAGE)
    parser.add_argument("--release", type=Path, help="Consume the existing publisher's release.json")
    args = parser.parse_args()
    image, standby = args.manager_image, None
    if args.release:
        manifest = json.loads(args.release.read_text())
        image = manifest["images"]["manager"]["pinned"]
        standby = manifest["images"].get("browser", {}).get("pinned")
    try:
        print(json.dumps(update(image, standby), indent=2), flush=True)
        return 0
    except Exception as error:
        # Do not expose API payloads containing environment or compose secrets.
        print(json.dumps({"status": "failed", "error_type": type(error).__name__, "error": str(error)[:400]}), flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
