#!/usr/bin/env python3
"""Single-port release plan and publisher. Uses committed inputs, never runtime data."""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import subprocess
import sys

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
                       "cache_ref": f"{image}:buildcache-{CHANNEL}-amd64",
                       "cache_scope": f"{CHANNEL}-{name}-amd64",
                       "labels": "\n".join(f"{k}={v}" for k, v in labels.items())})
    return {"schema": 1, "repository": repository, "source": source_url, "commit": commit,
            "platform": PLATFORM, "browser_key": key,
            "browser_inputs": [name for name, _, _ in inputs], "images": images}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan",))
    parser.add_argument("--repository", help="GitHub OWNER/REPO (default: origin)")
    parser.add_argument("--namespace", help="Docker Hub namespace (not inferred from GitHub owner)")
    parser.add_argument("--source", default="HEAD", help="Committed source ref (default: HEAD)")
    args = parser.parse_args()
    root = Path(git(Path.cwd(), "rev-parse", "--show-toplevel").decode().strip())
    print(json.dumps(make_plan(root, args.repository, args.namespace, args.source), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as error:
        sys.exit(f"release: {error}")
