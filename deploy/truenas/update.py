#!/usr/bin/env python3
"""Update the existing TN App through its official API; optionally apply an approved topology."""
import argparse
import copy
import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys

APP = "neko"
APPDATA = Path("/mnt/ylangsPool/appdata/neko")
DEFAULT_IMAGE = "docker.io/ylangylang/neko-rooms:single-port"
LEGACY = {"neko", "cdp-proxy"}


def prepare_config(original, image, standby_image=None, rendered=None):
    config = copy.deepcopy(original if rendered is None else rendered)
    if rendered is not None:
        known = LEGACY | {"neko-rooms", "gateway", "classifier", "turn", "room-image"}
        if set(original["services"]) - known:
            raise RuntimeError("Additional existing App services require restart-scope review")
        if not standby_image:
            raise ValueError("Topology migration requires the released browser image")
        # Current protected service definitions, not template defaults, remain authoritative.
        for name in LEGACY:
            config["services"][name] = copy.deepcopy(original["services"][name])
        if original.get("configs"):
            config.setdefault("configs", {}).update(copy.deepcopy(original["configs"]))
        config["services"]["room-image"]["image"] = standby_image
        config["services"]["neko-rooms"]["environment"]["NEKO_ROOMS_NEKO_IMAGES"] = standby_image
        for name, service in config["services"].items():
            if name not in LEGACY:
                service["pull_policy"] = "never"  # All images are pre-pulled before the maintenance window.
    config["services"]["neko-rooms"]["image"] = image
    return config


class DockerHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(10)
        self.sock.connect("/var/run/docker.sock")


def docker_get(path):
    connection = DockerHTTP("localhost", timeout=10)
    try:
        connection.request("GET", path)
        response = connection.getresponse()
        if response.status != 200:
            raise RuntimeError("Docker metadata request failed: " + str(response.status))
        return json.loads(response.read())
    finally:
        connection.close()


def container_snapshot():
    rows = []
    for entry in docker_get("/containers/json?all=1"):
        if not any("neko" in name.lower() for name in entry["Names"]):
            continue
        x = docker_get("/containers/" + entry["Id"] + "/json")
        browser_pid = None
        if x["State"]["Running"]:
            top = docker_get("/containers/" + entry["Id"] + "/top?ps_args=-eo%20pid,ppid,comm")
            browsers = [int(row[0]) for row in top.get("Processes", []) if row[-1] in ("chromium", "chrome")]
            browser_pid = min(browsers) if browsers else None
        rows.append({"name": x["Name"], "id": x["Id"], "image": x["Config"]["Image"],
                     "started": x["State"]["StartedAt"], "pid": x["State"]["Pid"],
                     "running": x["State"]["Running"], "paused": x["State"]["Paused"], "browser_pid": browser_pid,
                     "mounts": sorted(x["Mounts"], key=lambda m: m["Destination"]),
                     "ports": x["HostConfig"]["PortBindings"],
                     "project": (x["Config"].get("Labels") or {}).get("com.docker.compose.project")})
    return rows


def assert_preserved(before, after, allow_default_recreate=False):
    actual = {row["name"]: row for row in after}
    checked = []
    for old in before:
        external = "neko" in old["name"].lower() and old.get("project") != "ix-neko"
        default = old["name"] == "/ix-neko-neko-1"
        if not (external or default):
            continue
        fields = ["image", "ports", "mounts"]
        if external or not allow_default_recreate:
            fields += ["id", "started", "pid", "running", "paused"]
            if "browser_pid" in old:
                fields.append("browser_pid")
        new = actual.get(old["name"], {})
        for field in fields:
            if old.get(field) != new.get(field):
                raise RuntimeError("Protected container mismatch: " + old["name"] + " " + field)
        checked.append(old["name"])
    if not checked:
        raise RuntimeError("Protected container baseline is empty")
    return checked


def protected_files():
    paths = [APPDATA / name for name in ("rooms.env", "neko.env", "cdp.htpasswd",
             "config/chromium.conf", "config/chromium-policies.json")]
    return {str(p): {"sha256": hashlib.sha256(p.read_bytes()).hexdigest(), "inode": p.stat().st_ino,
                    "mode": p.stat().st_mode & 0o777} for p in paths}


def protected_directories():
    return {name: {"inode": (APPDATA / name).stat().st_ino, "uid": (APPDATA / name).stat().st_uid,
                   "gid": (APPDATA / name).stat().st_gid, "mode": (APPDATA / name).stat().st_mode & 0o777}
            for name in ("rooms-data", "profile", "downloads")}


def update(image, standby_image=None, compose=None, baseline=None):
    from truenas_api_client import Client

    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    backup = APPDATA / "backups" / ("manager-update-" + stamp)
    backup.mkdir(parents=True, mode=0o700)
    seen = set()

    def deliver(kind, reference):
        if not reference or reference in seen:
            return
        seen.add(reference)
        inspected = subprocess.run(["docker", "image", "inspect", reference], stdout=subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL)
        if inspected.returncode < 0:
            raise RuntimeError(kind + " image inspection was killed; stop before App update")
        if inspected.returncode != 0:
            with (backup / ("pull-" + kind + ".log")).open("w") as log:
                result = subprocess.run(["docker", "pull", reference], stdout=log, stderr=subprocess.STDOUT)
            if result.returncode:
                raise RuntimeError(kind + " pull failed; inspect private backup " + str(backup))

    # Channel tags must be refreshed; immutable references can reuse local delivery.
    if "@sha256:" not in image:
        with (backup / "pull-manager.log").open("w") as log:
            subprocess.run(["docker", "pull", image], stdout=log, stderr=subprocess.STDOUT, check=True)
    deliver("manager", image)
    deliver("browser", standby_image)
    with Client() as client:
        original = client.call("app.config", APP)
        (backup / "app-config.json").write_text(json.dumps(original, indent=2) + "\n")
        for name in ("compose.yaml", "rooms.env", "neko.env", "cdp.htpasswd"):
            if (APPDATA / name).is_file():
                shutil.copy2(APPDATA / name, backup / name)
                (backup / name).chmod(0o600)
        rendered = json.loads(compose.read_text()) if compose else None
        config = prepare_config(original, image, standby_image, rendered)
        manager = config["services"]["neko-rooms"]
        previous = original["services"]["neko-rooms"]["image"]
        if config == original and "@sha256:" in image:
            return {"status": "unchanged", "manager_image": image,
                    "url": manager.get("environment", {}).get("NEKO_ROOMS_INSTANCE_URL"),
                    "network_mode": manager.get("environment", {}).get("NEKO_ROOMS_NETWORK_MODE", "direct")}
        before = files = None
        if compose:
            if not baseline or not (baseline / "default-neko-nssdb").is_dir():
                raise ValueError("Topology migration requires a snapshot and whole-directory NSS backup")
            before = container_snapshot()
            # Default identity may differ after an authorized recovery; its
            # image/ports/mounts remain protected, as do all external identities.
            assert_preserved(json.loads((baseline / "containers-before.json").read_text()), before, True)
            files = protected_files()
            saved_state = json.loads((baseline / "protected-files.json").read_text())
            saved = saved_state["files"]
            for name in ("rooms.env", "neko.env", "cdp.htpasswd"):
                if any(files[str(APPDATA / name)][key] != saved[name][key] for key in ("sha256", "inode", "mode")):
                    raise RuntimeError("Protected auth file changed since baseline: " + name)
            directories = protected_directories()
            if directories != saved_state["directories"]:
                raise RuntimeError("Protected data directory changed since baseline")
            (backup / "containers-before.json").write_text(json.dumps(before, indent=2))
            (backup / "protected-files.json").write_text(json.dumps(files, indent=2))
            for name, service in config["services"].items():
                if name not in LEGACY:
                    deliver(name, service["image"])
        candidate = backup / "candidate-config.json"
        candidate.write_text(json.dumps(config, indent=2) + "\n")
        if compose:
            with (backup / "candidate-validation.log").open("w") as log:
                validated = subprocess.run(["docker", "compose", "--project-name", "ix-neko", "--env-file", "/dev/null",
                                            "-f", str(candidate), "config", "--quiet"], stdout=log, stderr=subprocess.STDOUT)
            if validated.returncode:
                raise RuntimeError("Final merged Compose invalid; inspect private backup " + str(backup))
        # Exactly one official update. Failure/mismatch never silently triggers a
        # second default-browser recreation; the operator owns recovery decisions.
        client.call("app.update", APP, {"custom_compose_config_string": json.dumps(config)}, job=True)
        if compose:
            after = container_snapshot()
            (backup / "containers-after.json").write_text(json.dumps(after, indent=2))
            checked = assert_preserved(before, after, allow_default_recreate=True)
            if files != protected_files() or directories != protected_directories():
                raise RuntimeError("Protected auth/config/data-root mismatch; stop before further action")
            (backup / "preservation-result.json").write_text(json.dumps({"checked": checked,
                 "auth_config_unchanged": True, "default_recreation_authorized": True}, indent=2))
        result = client.call("app.get_instance", APP)
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
               "owner_credentials": str(APPDATA / "rooms.env"), "rollback_config": str(backup / "app-config.json"),
               "preservation_result": str(backup / "preservation-result.json") if compose else None}
    (backup / "result.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manager-image", default=DEFAULT_IMAGE)
    parser.add_argument("--release", type=Path, help="Consume the existing publisher's release.json")
    parser.add_argument("--compose", type=Path, help="Approved rendered Compose JSON for this topology migration")
    parser.add_argument("--baseline", type=Path, help="Migration backup with container/auth snapshots and NSS copy")
    args = parser.parse_args()
    image, standby = args.manager_image, None
    if args.release:
        manifest = json.loads(args.release.read_text())
        image = manifest["images"]["manager"]["pinned"]
        standby = manifest["images"].get("browser", {}).get("pinned")
    try:
        print(json.dumps(update(image, standby, args.compose, args.baseline), indent=2), flush=True)
        return 0
    except Exception as error:
        print(json.dumps({"status": "failed", "error_type": type(error).__name__, "error": str(error)[:400]}), flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
