# syntax=docker/dockerfile:1.7-labs
# Deployment adapter only: upstream Neko core, UI, browser and startup stay intact.
FROM ghcr.io/m1k1o/neko/chromium:3.2.0@sha256:270face8b8111e639744fb7bcdbbf0deed227900bb89d58b2f8adfc12af8c41e

USER root
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && \
    apt-get install -y --no-install-recommends socat && \
    test -x /usr/bin/socat

COPY enable.py /tmp/neko-enable-cdp.py
RUN python3 /tmp/neko-enable-cdp.py && \
    rm /tmp/neko-enable-cdp.py && \
    install -d -m 0700 -o neko -g neko /home/neko/profile
COPY relay.conf /etc/neko/supervisord/cdp-relay.conf
# No EXPOSE or host publishing: only Manager's private bridge may reach :9223.
# CMD, healthcheck, API label, Neko binary, frontend and Chromium remain upstream.
