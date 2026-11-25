#!/usr/bin/env python3
"""Build-time delta to the pinned native Chromium launcher, not a new browser."""
import json
from pathlib import Path

SUPERVISOR = Path("/etc/neko/supervisord/chromium.conf")
POLICIES = Path("/etc/chromium/policies/managed/policies.json")
UPSTREAM_PROFILE = "  --user-data-dir=/home/neko/.config/chromium\n"
CDP_PROFILE = ("  --user-data-dir=/home/neko/profile\n"
               "  --remote-debugging-address=127.0.0.1\n"
               "  --remote-debugging-port=9222\n")


def enable(supervisor, policies):
    """Preserve native argv, supervision and every unrelated managed policy."""
    if (supervisor.count(UPSTREAM_PROFILE) != 1
            or "command=/usr/bin/chromium\n" not in supervisor
            or "--remote-debugging" in supervisor
            or "--remote-allow-origins" in supervisor):
        raise ValueError("pinned native Chromium launcher changed; review the CDP adapter")
    if policies.get("DeveloperToolsAvailability") != 2:
        raise ValueError("pinned Chromium DevTools policy changed; review the CDP adapter")
    updated = dict(policies, DeveloperToolsAvailability=0, RemoteDebuggingAllowed=True)
    return supervisor.replace(UPSTREAM_PROFILE, CDP_PROFILE), updated


def main():
    supervisor, policies = enable(SUPERVISOR.read_text(), json.loads(POLICIES.read_text()))
    SUPERVISOR.write_text(supervisor)
    POLICIES.write_text(json.dumps(policies, indent=2) + "\n")


if __name__ == "__main__":
    main()
