#!/usr/bin/env python3
"""Single-port release plan and publisher. Uses committed inputs, never runtime data."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import subprocess
import sys
import tempfile

PLATFORM = "linux/amd64"
BROWSER_CONTEXT = "examples/single-port-relay/room-cdp"
CHANNEL = "single-port"


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args])


def browser_inputs(root, commit):
    """Resolve literal local COPY sources; fail rather than miss a new build input."""
    tree = {}
    for entry in git(root, "ls-tree", "-rz", commit, "--", BROWSER_CONTEXT).split(b"\0"):
        if entry:
            meta, name = entry.split(b"\t", 1)
            mode, kind, _ = meta.decode().split()
            if kind != "blob" or mode not in ("100644", "100755"):
                raise ValueError("Browser context must contain regular tracked files")
            tree[name.decode().removeprefix(BROWSER_CONTEXT + "/")] = mode
    dockerfile = git(root, "show", f"{commit}:{BROWSER_CONTEXT}/Dockerfile")
    selected = {"Dockerfile"}
    if ".dockerignore" in tree:
        selected.add(".dockerignore")
    # The adapter uses literal COPYs. Unsupported syntax must not produce a stale key.
    for line in dockerfile.decode().splitlines():
        if not re.match(r"^\s*(COPY|ADD)\s", line, re.I):
            continue
        parts = shlex.split(line)
        if parts[0].upper() != "COPY" or len(parts) < 3:
            raise ValueError("Release planner supports literal local COPY only")
        for source in parts[1:-1]:
            if source.startswith(("--", "/")) or any(c in source for c in "*$?[]\\"):
                raise ValueError(f"Unsupported browser COPY source: {source}")
            source = str(PurePosixPath(source))
            if ".." in PurePosixPath(source).parts:
                raise ValueError("Browser COPY must stay inside its build context")
            matches = {p for p in tree if source == "." or p == source or p.startswith(source.rstrip("/") + "/")}
            if not matches:
                raise ValueError(f"Untracked/missing browser COPY input: {source}")
            selected.update(matches)
    return [(name, tree[name], git(root, "show", f"{commit}:{BROWSER_CONTEXT}/{name}"))
            for name in sorted(selected)]


def browser_key(inputs, platform=PLATFORM):
    digest = hashlib.sha256(b"neko-room-cdp-v1\0" + platform.encode() + b"\0")
    for name, mode, content in inputs:
        digest.update(name.encode() + b"\0" + mode.encode() + b"\0")
        digest.update(str(len(content)).encode() + b"\0" + content + b"\0")
    return digest.hexdigest()


def repository_name(root, explicit=None):
    value = explicit or os.environ.get("GITHUB_REPOSITORY")
    if not value:
        remote = git(root, "remote", "get-url", "origin").decode().strip()
        match = re.fullmatch(r"(?:https://github\.com/|git@github\.com:)([^/]+/[^/]+?)(?:\.git)?", remote)
        if not match:
            raise ValueError("Pass --repository OWNER/REPO for the source repository")
        value = match[1]
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", value):
        raise ValueError("Repository must be OWNER/REPO")
    return value.lower()


def make_plan(root, repository=None, namespace=None, source="HEAD"):
    repository = repository_name(root, repository)
    namespace = namespace or os.environ.get("DOCKERHUB_NAMESPACE")
    if not namespace and repository == "ylang-ylang/neko-rooms":
        namespace = "ylangylang"  # Verified Docker Hub account; not the GitHub owner.
    if not namespace or not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", namespace):
        raise ValueError("Set --namespace (or DOCKERHUB_NAMESPACE) to your Docker Hub namespace")
    commit = git(root, "rev-parse", "--verify", f"{source}^{{commit}}").decode().strip()
    inputs = browser_inputs(root, commit)
    key = browser_key(inputs)
    source_url = f"https://github.com/{repository}"
    cache_platform = PLATFORM.replace("/", "-")
    images = []
    for name, image, suffix, context, input_key in (
        ("manager", "neko-rooms", commit[:7], ".", commit),
        ("browser", "neko-chromium-cdp", key[:12], BROWSER_CONTEXT, key),
    ):
        image = f"docker.io/{namespace}/{image}"
        labels = {"org.opencontainers.image.source": source_url,
                  "org.opencontainers.image.revision": commit,
                  "io.neko-rooms.release.input": input_key}
        images.append({"name": name, "image": image, "tag": f"{image}:{CHANNEL}-{suffix}",
                       "channel": f"{image}:{CHANNEL}", "context": context,
                       "cache_ref": f"{image}:buildcache-{CHANNEL}-{cache_platform}",
                       "cache_scope": f"{CHANNEL}-{name}-{cache_platform}",
                       "labels": "\n".join(f"{k}={v}" for k, v in labels.items())})
    return {"schema": 1, "repository": repository, "source": source_url, "commit": commit,
            "platform": PLATFORM, "browser_key": key,
            "browser_inputs": [name for name, _, _ in inputs], "images": images}


def remote_digest(reference):
    """Inspect manifests only: never pull Chromium layers or hide authentication failures."""
    result = subprocess.run(["docker", "buildx", "imagetools", "inspect", reference,
                             "--format", "{{json .Manifest}}"], capture_output=True, text=True)
    if result.returncode:
        error = result.stderr.strip()
        if re.search(r"unauthorized|forbidden|denied|insufficient_scope|429|timeout|no such host", error, re.I):
            raise RuntimeError(f"Registry probe failed for {reference}; check registry access/bootstrap: {error}")
        if re.search(r"manifest unknown|name unknown|not found|\b404\b", error, re.I):
            return None
        raise RuntimeError(f"Registry probe failed for {reference}: {error}")
    digest = json.loads(result.stdout).get("digest", "")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError(f"Registry returned an invalid digest for {reference}")
    return digest


def probe_plan(plan):
    for image in plan["images"]:
        image["digest"] = remote_digest(image["tag"])
        image["build"] = image["digest"] is None
    return plan


def write_json(path, value):
    Path(path).write_text(json.dumps(value, indent=2) + "\n")


def emit_plan(plan, output=None, github_output=None):
    if output:
        write_json(output, plan)
    else:
        print(json.dumps(plan, indent=2))
    if github_output:
        if len(plan["images"]) != 1:
            raise ValueError("--github-output requires --image manager|browser")
        # These are STEP outputs, never job outputs: a secret Docker Hub username
        # in an image ref would cause Actions to suppress a cross-job output.
        values = dict(plan["images"][0], platform=plan["platform"])
        with open(github_output, "a") as stream:
            for key, value in values.items():
                if isinstance(value, bool):
                    value = str(value).lower()
                value = "" if value is None else value
                stream.write(f"{key}<<neko_release_{key}\n{value}\nneko_release_{key}\n")


def check_commands(root, component):
    if component == "go":
        return [(root, ["go", "test", "./..."])]
    if component == "adapter":
        return [(root, [sys.executable, "-B", "-m", "unittest", "discover", "-s",
                        "examples/single-port-relay", "-p", "test_room_cdp.py", "-v"])]
    tests = sorted(str(p.relative_to(root / "client")) for p in (root / "client/tests").glob("*.test.cjs"))
    if not tests:
        raise ValueError("No frontend tests found")
    return [(root / "client", command) for command in (
        ["npm", "ci"], ["npm", "run", "lint", "--", "--no-fix"], ["node", "--test", *tests])]


def check(root, component):
    for directory, command in check_commands(root, component):
        subprocess.run(command, cwd=directory, check=True)


def build_command(plan, image, root, builder=None):
    command = ["docker", "buildx", "build", "--push", "--platform", plan["platform"],
               "--tag", image["tag"], "--cache-from", f"type=registry,ref={image['cache_ref']}",
               "--cache-to", f"type=registry,ref={image['cache_ref']},mode=max", "--progress", "plain"]
    if builder:
        command.extend(["--builder", builder])
    for label in image["labels"].splitlines():
        command.extend(["--label", label])
    return command + [str(root / image["context"])]


def build_image(plan, image, root, builder):
    # Check again after the plan probe: immutable tags are reused, never overwritten by a rerun.
    if remote_digest(image["tag"]) is None:
        subprocess.run(build_command(plan, image, root, builder), check=True)


@contextmanager
def snapshot(root, commit):
    """Manual publishing always builds the exact Git commit, not a dirty checkout."""
    with tempfile.TemporaryDirectory(prefix="neko-release-") as directory:
        directory = Path(directory)
        archive = directory / "source.tar"
        source = directory / "source"
        source.mkdir()
        with archive.open("wb") as stream:
            subprocess.run(["git", "-C", str(root), "archive", "--format=tar", commit], stdout=stream, check=True)
        subprocess.run(["tar", "-xf", str(archive), "-C", str(source)], check=True)
        yield source


def ensure_builder(builder):
    result = subprocess.run(["docker", "buildx", "inspect", builder], capture_output=True, text=True)
    if result.returncode:
        if not re.search(r"no builder|does not exist|not found", result.stderr, re.I):
            raise RuntimeError(f"Cannot inspect release builder: {result.stderr.strip()}")
        subprocess.run(["docker", "buildx", "create", "--name", builder, "--driver", "docker-container"], check=True)


def assert_current(root, plan, expected_ref):
    if not expected_ref.startswith("refs/heads/"):
        raise ValueError("Channel promotion requires a branch ref, not a detached/tag ref")
    remote = git(root, "ls-remote", "--exit-code", "origin", expected_ref).decode().split()
    if not remote or remote[0] != plan["commit"]:
        raise RuntimeError(f"Refusing stale promotion: {expected_ref} no longer points to {plan['commit']}")


def release_manifest(plan):
    images = {}
    # Resolve BOTH immutable artifacts before any channel write.
    for image in plan["images"]:
        digest = remote_digest(image["tag"])
        if digest is None:
            raise RuntimeError(f"Cannot release: missing immutable artifact {image['tag']}")
        images[image["name"]] = {"immutable": image["tag"], "channel": image["channel"],
                                  "digest": digest, "pinned": f"{image['image']}@{digest}"}
    return {"schema": 1, "source": plan["source"], "commit": plan["commit"],
            "platform": plan["platform"], "browser_key": plan["browser_key"],
            "channels_promoted": False, "images": images}


def summary(manifest):
    lines = ["## Single-port release", "", f"Source: {manifest['source']}/commit/{manifest['commit']}",
             f"Platform: `{manifest['platform']}`; channels promoted: `{manifest['channels_promoted']}`", "",
             "| Component | Immutable tag | Digest-pinned ref | Channel |", "| --- | --- | --- | --- |"]
    for name, image in manifest["images"].items():
        lines.append(f"| {name} | `{image['immutable']}` | `{image['pinned']}` | `{image['channel']}` |")
    return "\n".join(lines) + "\n"


def promote(root, plan, output, summary_path=None, expected_ref=None):
    manifest = release_manifest(plan)
    if expected_ref:
        assert_current(root, plan, expected_ref)
    write_json(output, manifest)
    for image in manifest["images"].values():
        subprocess.run(["docker", "buildx", "imagetools", "create", "--prefer-index=false",
                        "--tag", image["channel"], image["pinned"]], check=True)
    # Successful registry writes are the handoff; no post-release image pulls or
    # deployment acceptance matrix. --prefer-index=false preserves source digests.
    manifest["channels_promoted"] = True
    write_json(output, manifest)
    if summary_path:
        with open(summary_path, "a") as stream:
            stream.write(summary(manifest))
    print(summary(manifest))
    return manifest


def publish(root, plan, args):
    if args.dry_run:
        preview = dict(plan)
        preview["dry_run"] = True
        preview["note"] = "No registry probes, builds or channel writes. publish will probe and reuse immutable tags."
        preview["build_commands_if_missing"] = [build_command(plan, i, Path("<committed-source-snapshot>"), args.builder)
                                                 for i in plan["images"]]
        emit_plan(preview, args.output)
        return
    probe_plan(plan)  # Fail fast on authentication/network errors, before costly builds.
    # CI (build.yml) runs the scripts/Go/frontend/adapter checks and gates channel
    # promotion on them. The manual publisher only produces missing artifacts;
    # Go and the UI compile inside Docker, so no host Go/Node/npm is required.
    with snapshot(root, plan["commit"]) as source:
        missing = [image for image in plan["images"] if image["build"]]
        if missing:
            ensure_builder(args.builder)
            with ThreadPoolExecutor(max_workers=2) as pool:
                builds = [pool.submit(build_image, plan, image, source, args.builder) for image in missing]
                for future in builds:
                    future.result()
    promote(root, plan, args.output or "release.json", args.summary, args.expected_ref)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "check", "manifest", "promote", "publish"))
    parser.add_argument("--repository", help="GitHub OWNER/REPO (default: origin)")
    parser.add_argument("--namespace", help="Docker Hub namespace (not inferred from GitHub owner)")
    parser.add_argument("--source", default="HEAD", help="Committed source ref (default: HEAD)")
    parser.add_argument("--probe", action="store_true", help="Plan: inspect remote immutable manifests, no layer pulls")
    parser.add_argument("--dry-run", action="store_true", help="Publish: print an offline plan without side effects")
    parser.add_argument("--output", help="JSON file (publish/promote/manifest default: release.json)")
    parser.add_argument("--github-output", help="Plan: append selected image STEP outputs to this Actions file")
    parser.add_argument("--image", choices=("manager", "browser"), help="Plan: select one image")
    parser.add_argument("--summary", help="Append a human-readable release table to this file")
    parser.add_argument("--component", choices=("go", "client", "adapter"), help="Check: required component")
    parser.add_argument("--builder", default="neko-release", help="Manual publisher's persistent buildx builder")
    parser.add_argument("--expected-ref", help="Promotion: require origin's branch tip to match source")
    args = parser.parse_args()
    if args.dry_run and args.command != "publish":
        parser.error("--dry-run is only valid with publish; plan is already offline")
    if (args.probe or args.image or args.github_output) and args.command != "plan":
        parser.error("--probe, --image and --github-output are only valid with plan")
    root = Path(git(Path.cwd(), "rev-parse", "--show-toplevel").decode().strip())
    if args.command == "check":
        if not args.component:
            parser.error("check requires --component go|client|adapter")
        check(root, args.component)
        return
    plan = make_plan(root, args.repository, args.namespace, args.source)
    if args.command == "plan":
        if args.image:
            plan["images"] = [image for image in plan["images"] if image["name"] == args.image]
        emit_plan(probe_plan(plan) if args.probe else plan, args.output, args.github_output)
    elif args.command == "manifest":
        manifest = release_manifest(plan)
        write_json(args.output or "release.json", manifest)
        print(summary(manifest))
    elif args.command == "promote":
        promote(root, plan, args.output or "release.json", args.summary, args.expected_ref)
    else:
        publish(root, plan, args)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"release: {error}")
