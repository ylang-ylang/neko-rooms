# Fixed-public-port relay example

A **colocated, private Docker bridge** deployment with two public TCP listeners:

| Listener (overridable) | Gateway protocol | Private upstream |
| --- | --- | --- |
| HTTPS **443** | HTTP + WebSocket over TLS | manager `neko-rooms:8080` |
| TURN/TLS **5349** | **TCP stream**, not HTTP | coturn `turn:3478` |

Coturn allocates ordinary UDP relay sockets **only on its private room-network IP**. Its configurable `49160–49259` allocation pool is **not published on the host, forwarded by the router, or opened on a VPS**. Neko rooms advertise private candidates and have no Docker host port bindings in `relay` mode. There is no per-room public port range and no experimental `--multiplex-peer` mechanism.

**This is not a public deployment.** The user's gateway machine, actual domain and matching public-trust certificate have not been supplied. They are required inputs; neither this example nor its tests substitutes a fabricated public URL. No DNS, WAN firewall, SSH or TrueNAS change is performed here. The default bind address is loopback for staging; public activation is an explicit operator change on the approved gateway.

This directory belongs to the **source fork**, not the NAS application layout. For the later NAS integration, merge the relevant service/config blocks into the existing **flat root `neko.compose.yaml`**, then submit the Custom App YAML through the official TrueNAS `app.update` workflow. Do not use manual partial Compose commands or edit TrueNAS internal records. Do not create another App directory/worktree, deploy a second manager, or replace existing storage/network settings with this example's fixture settings.

## Requirements and image contract

- Local Docker Engine, Docker Compose **2.30+** (`env_file.format: raw`), Python **3.9+**, OpenSSL **3+**, and an existing RFC1918 IPv4 Docker bridge. Fixture tests also use `iproute2`.
- Relay-capable manager release **`truenas-neko-rooms-agent:1.6.5-3`** must be built/preloaded first. It is a locally built release, not an assumed public registry image. `pull_policy: never` makes absence explicit. Build/release from the repository's existing root `Dockerfile`: compiled management UI + statically built Go binary in the existing scratch final image. Do not replace it with upstream `latest`, an old manager that ignores relay mode, or a shell-wrapper image.
- Keep room image **`truenas-neko-chromium-cdp:3.2.0-1`**: official Neko **3.2.0** core plus the existing CDP relay. Preload the approved local room release as well; the manager does not pull it. No Neko rebuild/core modification is required.
- Nginx **1.30.7-alpine** and BSD-licensed coturn **4.6.3** are pinned by digest in `compose.yaml`. Coturn's default image entrypoint (including automatic public-IP detection) is deliberately bypassed.

For release archives supplied by the existing release workflow:

```sh
docker load -i "${MANAGER_RELEASE_ARCHIVE:?set approved local archive}"
docker load -i "${ROOM_RELEASE_ARCHIVE:?set approved local archive}"
docker image inspect truenas-neko-rooms-agent:1.6.5-3 >/dev/null
docker image inspect truenas-neko-chromium-cdp:3.2.0-1 >/dev/null
```

Check the manager's release provenance and relay feature before activation. The default remains `NEKO_ROOMS_NETWORK_MODE=direct` outside this example. Backend network policy applies only to new/recreated manager-created rooms, not running dynamic rooms; TrueNAS App-owned services have the separate update lifecycle described below.

## 1. Supply actual private configuration

Run from `examples/relay/`. Keep operator inputs, generated configs and private keys **outside the checkout**, on the Docker host:

```sh
PRIVATE=${PRIVATE:?set an absolute private directory outside the checkout}
mkdir -p "$PRIVATE"
chmod 700 "$PRIVATE"
install -m 600 .env.example "$PRIVATE/relay.env"
# Edit $PRIVATE/relay.env with a native editor; never paste owner secrets into an Agent prompt.
```

The input file uses literal `KEY=value` lines, optionally surrounded by matching quotes. It is **not a shell script**: no interpolation, escape processing, inline comments or multiline values. Do not `source` it. This preserves special characters in the existing owner's password.

Required values:

| Variable | Actual value to supply |
| --- | --- |
| `PUBLIC_HOST` | Actual DNS hostname, **no scheme/path/port**; not a documentation domain |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Absolute PEM full-chain/key paths; key mode `600`; certificate must match hostname, be unexpired and key must match |
| `PUBLIC_BIND_IP` | `127.0.0.1` for staging; approved gateway bind IP for activation |
| `HTTPS_PORT`, `TURNS_PORT` | Default `443`, `5349`; distinct TCP ports; origin and TURN URI are generated with the selected ports |
| `ROOM_NETWORK`, `ROOM_NETWORK_CIDR` | Real existing Docker bridge name and its actual subnet |
| `TURN_PRIVATE_IP` | Unused static IPv4 address in that bridge, not its gateway; preferably outside its dynamic `IPRange` |
| `STORAGE_PATH`, `INSTANCE_NAME` | Existing absolute host storage root and manager instance name; keep them unchanged during integration |
| `OWNER_USERNAME`, `OWNER_PASSWORD` | **Existing configured owner only**, not new credentials |

`DOCKER_SOCKET` defaults to `/var/run/docker.sock`; it must be the daemon that owns the bridge/room containers. The manager's `/data` bind maps back to **the identical host `STORAGE_PATH`** via `NEKO_ROOMS_STORAGE_EXTERNAL`; Docker uses that host path for dynamically created room mounts. Profile (`/home/neko/profile`) and Downloads (`/home/neko/Downloads`) remain the existing separate private presets. Disabling persistence affects Profile only. Do not move data, change ownership wholesale, or migrate existing rooms as part of networking activation.

Use `docker network inspect "$ROOM_NETWORK"` and `ip route` to inspect the actual host. The network name/CIDR are deliberately required, not defaulted from another host; reuse the existing installation's actual values when integrating. For a new dedicated bridge, first choose a non-overlapping subnet/dynamic pool and static TURN address, then explicitly create it:

```sh
docker network create --driver bridge \
  --subnet "${ROOM_NETWORK_CIDR:?choose non-overlapping subnet}" \
  --ip-range "${ROOM_DYNAMIC_CIDR:?choose pool excluding TURN_PRIVATE_IP}" \
  "${ROOM_NETWORK:?choose dedicated network name}"
```

The bridge need not be `internal: true`: browser rooms still need outbound Internet access. It is private because its addresses/listeners are not WAN-published. Use a **room-dedicated trusted bridge**; coturn's peer allowlist grants the whole specified CIDR, not per-room authorization.

```sh
python3 configure.py --env-file "$PRIVATE/relay.env" --output-dir "$PRIVATE/runtime"
STATE="$PRIVATE/runtime"
dc() { docker compose --env-file "$STATE/compose.env" -f "$PWD/compose.yaml" "$@"; }
dc config --quiet
dc run --rm --no-deps gateway -t
```

`configure.py` validates required domain/cert/key, the actual Docker subnet, static address, storage and socket before rendering. Missing inputs fail explicitly. No services are started and no networks/storage/rooms are created. `gateway -t` checks the **mounted complete Nginx configuration**, including TLS and stream support; it opens no public listener.

Private generated files (directory `700`, files `600`):

- `compose.env`: resolved topology and paths; use with **every** `dc` command.
- `nginx.conf`: HTTP and TCP-stream TLS gateway, paths preserved.
- `manager.env`: existing owner credentials and `NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND` JSON.
- `turnserver.conf`, `turn-credentials.json`: generated strong TURN-only long-term credentials. Blank TURN inputs generate once and reuse this private file; repeated configure does not silently rotate credentials.

Do not print `dc config` or container environment dumps into shared logs: resolved output contains credentials. `config --quiet` is sufficient for operator syntax checking.

## 2. Verify locally, without a real room or public listener

The standalone fixture test uses temporary self-signed **`.test`-only** certificates, random **loopback** TCP ports, a scratch private bridge and an HTTP/WebSocket echo fixture instead of the manager. It neither starts Neko nor mounts the Docker socket/storage into the fixture container. Its only media-related operation is a TURN protocol allocation/permission check, not a video experiment.

Preload the exact fixture/deployment images, then run:

```sh
docker pull nginx:1.30.7-alpine@sha256:caf4e1e032a20f9c3c5e28fc7f25ffd768ad8677b4975c54193075997b3a97e3
docker pull coturn/coturn:4.6.3@sha256:71c3c990283385567f11794ee692e3a47b66fd9b0bb39e42afbe776e331dd888
docker pull python:3.13-alpine@sha256:9ef1a599f863231c3d91e941389bfdc4a2d6070557243d54df1a4e57015b960e
python3 validate.py
```

The test checks Compose rendering, missing domain/cert/key and mismatched-certificate failures, rejection of fixture WAN binds and production test-DNS, private config permissions, `nginx -t`, verified TLS handshakes, root redirect, path/Host/forwarded-scheme/Bearer preservation, DevTools WebSocket `101`, and exactly two runtime TCP mappings. TURN checks must show: unauthenticated/wrong-password allocations rejected; correct credentials accepted; advertised relay address is private; public peer permission **403**; room-CIDR permission succeeds; TCP relay **442**. Scratch keys/configs and test containers/network are removed afterward. These results establish gateway/TURN plumbing, **not public reachability or Neko video**.

## 3. Controlled activation and negative checks

Do not apply this as a second Compose project to an existing manager instance. Existing NAS installation changes belong in its existing flat root YAML and await explicit gateway/domain/certificate input. **Standalone Compose only:** for a fresh colocated installation with approved real inputs, `dc up -d turn neko-rooms gateway` starts these three services; dynamic rooms are not Compose services and are not recreated by this command. This is **not** the activation procedure for a TrueNAS-managed App.

For an existing installation:

1. Privately save its current flat YAML, network/NAT settings and storage paths. Retain the previous manager image and current dynamic direct-room container IDs. Preserve the default browser's Profile/Downloads mounts and existing private credentials. **Manager restart invalidates owner sessions and Agent keys**.
2. Preload/verify the relay manager and unchanged room image. Check configuration with the staging bind, then merge the service/config changes into the existing deployment. Do not change the instance name/network/storage or run two managers concurrently.
3. Set the approved gateway bind IP and real public origin/certificate. Permit **only** HTTPS/TCP and TURN/TLS/TCP (default `443`, `5349`) at the approved public entry point. There is no requirement for port 80, UDP 3478, a UDP relay WAN range or per-room forwards. Certificate issuance/renewal is external to this example (e.g. DNS validation); no ACME account/domain is invented.
4. Apply using the platform's lifecycle:
   - **Standalone Compose:** start/restart only the manager and gateway/TURN services.
   - **TrueNAS 25.04 Custom App:** submit the revised Custom App YAML via official **`app.update`** (or the Custom App editor backed by it), not manual partial Compose commands or internal-record edits. The official update runs `down --remove-orphans`, then `up --force-recreate` for **all App services**, including the App-owned default Neko browser and CDP/gateway services. Expect a brief default-browser interruption as well as invalidated owner sessions/Agent keys; retaining the existing mounts and private credentials preserves Profile/Downloads data and login configuration.
5. Manager-created dynamic children (for example `work`) lack the App's Compose labels and remain running through that TrueNAS update. They are **not automatically recreated or migrated**; the backend network policy affects only new/recreated dynamic rooms. Create **one explicitly chosen new relay canary** through the existing owner form and verify it before opting any existing dynamic room into recreation.

With `ORIGIN` set to the actual generated `https://host[:HTTPS_PORT]` and hostname/port shell variables matching the actual configuration:

```sh
curl --fail --silent --show-error "$ORIGIN/manage/auth/status"
curl --silent --show-error -D - -o /dev/null "$ORIGIN/rooms/no-such-room/json/version"
openssl s_client -connect "$PUBLIC_HOST:$TURNS_PORT" -servername "$PUBLIC_HOST" \
  -verify_hostname "$PUBLIC_HOST" -verify_return_error </dev/null
docker inspect "$CANARY_CONTAINER" --format '{{json .HostConfig.PortBindings}}'
docker port "$CANARY_CONTAINER"
```

Expected negative checks:

- Without owner login, auth status is `mode: form`, `authenticated: false`; management uses the **existing form**, not a new global Basic/SSO challenge. A successful HTTPS form login sets the host-only HttpOnly/SameSite owner cookie **Secure**. Owner mutations require matching `Origin` including a nonstandard HTTPS port. `NEKO_ROOMS_PROXY=true` trusts the gateway's overwritten scheme/Host headers.
- Agent discovery **without** Bearer returns `401`, not a login HTML page; a valid temporary key works for both discovery and DevTools WebSocket. Revoke/expiry closes access. Agent keys never become owner cookies, and no owner secret appears in the handoff prompt. Nginx never challenges `/rooms/` itself.
- Relay canary `HostConfig.PortBindings` is empty (`null` or `{}`) and `docker port` is blank. Existing dynamic direct-room bindings remain unchanged. Browser ICE diagnostics must select a TURN relay candidate and receive advancing video; a successful TLS handshake alone is **not** that check. Real Neko/video acceptance is a separate canary/topology validation.
- Only the two selected public TCP listeners are reachable. Do not publish unauthenticated CDP `9222/9223`, manager `8080`, Docker `2375/2376` or the TrueNAS admin UI. Existing legacy/default-ID Basic CDP may remain LAN-only; this example does not remap it publicly. The gateway rejects unknown HTTP paths/Host and forwards only `/manage/`, `/room/` and `/rooms/` to the manager.
- TURN must reject missing/bad credentials and peer IPs outside the configured room CIDR. Do not remove `lt-cred-mech`, either deny-all rule or `no-tcp-relay`. TURN credentials necessarily reach authenticated room browsers: they are separate from owner/Agent secrets. Owner-only Compose exports also contain the instance-wide TURN credentials copied into relay rooms (alongside existing room passwords); store exports privately and redact them before sharing. CIDR restrictions plus allocation/bandwidth quotas prevent their use as a generic public UDP proxy; they do not isolate peers within the room CIDR. Default quotas are 100 allocations, 12.5 MB/s per allocation, 125 MB/s aggregate, not 100 rooms; size the private pool/quotas for actual viewers.

Rotate TURN credentials by supplying a new strong pair in the private input and re-running configure, then update/recreate only the chosen dynamic rooms whose copied frontend ICE settings must change. Keep both sides synchronized; restarting TURN drops active allocations. **Standalone Compose:** restart TURN for rotation; after validating renewed certificate/key files mounted by file bind, recreate only the gateway to pick up atomic certificate replacement (`dc up -d --no-deps --force-recreate gateway`). **TrueNAS Custom App:** use the official `app.update`/Custom App YAML workflow above and plan for recreation of all App services, not partial Compose restarts. Do not mass-recreate dynamic rooms.

### Rollback

Before stopping TURN, stop/remove the relay canary (and explicitly handle any other relay rooms) using the owner UI. Restore the previous manager configuration/image, `direct` mode and original LAN URLs/NAT settings in the existing deployment; remove the relay frontend setting there.

- **Standalone Compose:** restart only the manager, then stop the added gateway/TURN services when no relay room depends on them.
- **TrueNAS Custom App:** submit the restored full Custom App YAML through official **`app.update`**. Do not roll back with manual partial Compose commands or internal-record edits. The same `down --remove-orphans` / `up --force-recreate` lifecycle recreates all App services, briefly restarting the default browser and invalidating owner sessions/Agent keys. Keep its Profile/Downloads mounts and private credentials unchanged.

Running dynamic direct rooms retain their original bindings/data; a dynamic relay room does **not** acquire direct bindings just because the manager switches back. Backend policy applies only on new/explicitly selected recreation, not automatic migration of existing dynamic rooms. Do not delete the external network, room storage or private credential backup; do not use broad `down --volumes`/room deletion commands.

## Why the native Neko 3.2 credentials work

Official Neko **v3.2.0**, revision `92420ca8e4709c1ac49961c4bf5d3cc786d44f0d`, already supports standard authenticated frontend TURN:

- [`server/pkg/types/webrtc.go`](https://github.com/m1k1o/neko/blob/v3.2.0/server/pkg/types/webrtc.go): `ICEServer` has JSON `urls`, `username`, `credential`.
- [`server/internal/config/webrtc.go`](https://github.com/m1k1o/neko/blob/v3.2.0/server/internal/config/webrtc.go): parses `webrtc.iceservers.frontend` / `NEKO_WEBRTC_ICESERVERS_FRONTEND` JSON.
- [`server/internal/websocket/handler/signal.go`](https://github.com/m1k1o/neko/blob/v3.2.0/server/internal/websocket/handler/signal.go): includes the list in `signal/provide`.
- [`client/core/src/transport/webrtc.ts`](https://github.com/m1k1o/neko/blob/v3.2.0/client/core/src/transport/webrtc.ts): `onProvide` passes it unchanged to `new RTCPeerConnection({iceServers: iceservers ?? []})`.

The manager contract copies its global `NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND` into **new relay rooms**. A relay-only startup adapter writes a minimal native Neko YAML with `webrtc.ip_retrieval_url: ""` and `webrtc.nat1to1: []`, sets `NEKO_CONFIG`, then execs the room image's original Entrypoint + Cmd. This is required because pinned Neko 3.2 does not allow an empty environment variable to override its default public-IP fetch (`AllowEmptyEnv` is not enabled). The adapter keeps the official core/image unchanged; it is not a host secret file or a replacement media service. The temporary YAML is nonsecret and mode `0444`, so Neko can read it after dropping to UID1000. **Compatibility limit:** relay mode rejects custom `NEKO_CONFIG`, mounted/inherited native config files, and image CLI overrides of manager-owned Neko settings rather than hiding them; keep those images/settings in direct mode until the conflict is resolved. Unsupported recreation is rejected before removing the existing room.

Relay rooms default `NEKO_LOG_LEVEL=warn` because pinned Neko logs frontend TURN credentials at info/debug. An explicit per-room log-level override is an operator opt-in to that credential-bearing logging; keep those logs private. Direct-mode logging is unchanged.

No Neko core patch, alternate media framework or per-room public address mapping is needed. Coturn's documented [4.6.3 allow/deny precedence](https://github.com/coturn/coturn/blob/4.6.3/examples/etc/turnserver.conf) permits only the private room CIDR over ordinary UDP allocations; [`stream` TLS](https://nginx.org/en/docs/stream/ngx_stream_ssl_module.html) handles the public TURN client connection separately from [HTTP WebSocket proxying](https://nginx.org/en/docs/http/websocket.html).

## A VPS gateway is not private connectivity

This Compose example is for **one Docker host**. A public VPS does not magically reach a NAS Docker bridge. A split installation requires an actual private tunnel/routed network, non-overlapping CIDRs, bidirectional routes and firewall permission between the TURN relay IP/private UDP pool and the NAS room candidate IPs. The room must be able to send back to the **advertised private TURN relay address**; the TURN server must reach the room's private candidate. Account for Docker forwarding/NAT deliberately and validate the selected ICE pair.

Either keep coturn beside the rooms and privately tunnel the gateway's TCP upstream to it, or provide real routed TURN-to-room connectivity if TURN is remote. Neither design calls for publishing the UDP allocation range on the VPS. This example provisions no VPN/routes and claims no split-host or WAN deployment; the user's gateway/domain/certificate and private route must be supplied before that integration.
