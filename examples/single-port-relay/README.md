# Native Neko, HTTPS + TURN/TLS on one TCP port

Open **`https://neko.localhost:8443/manage/`**, create a room, then use **`https://neko.localhost:8443/room/native/`**. The viewer is the room image's **original Neko 3.2 UI and WebRTC**, including its native controls, audio, clipboard and other features. No copied frontend, MSE player, `aggregate.html`, or Manager `http` mode is used.

This is a **fresh, local, single-Docker-host example** in the Manager project. It does not modify an existing installation, NAS, Neko server, room image or management UI. [The two-port example](../relay/) remains separate.

```text
one published TCP socket: 127.0.0.1:8443 -> gateway:443
                                   TLS ClientHello preread (no termination)
    web SNI = neko.localhost ------> 127.0.0.1:8443 HTTPS -> neko-rooms:8080
    turn.localhost / NO SNI -------> 127.0.0.1:5349 TURN/TLS -> turn:3478 TCP
                                                              |
                                     private UDP allocations <-> native rooms
```

The loopback addresses above are **inside the gateway container**, not additional host bindings. Only its `443/tcp` is published. Coturn, Manager and Manager-created relay rooms publish no host ports; the UDP allocation pool is private. Docker DNS is re-resolved for Manager/coturn after container recreation.

There are **two DNS names, one IP and one external port**. Web clients **must send the application hostname as SNI**; an IP-only HTTPS URL cannot select the web branch. Chromium's TURN/TLS client can omit SNI, so the default branch **must be TURN**, not HTTPS. Public ingress must forward **raw TCP at layer 4**, preserving ClientHello; an ordinary HTTP reverse proxy cannot carry TURN.

## Requirements

- Local Linux Docker Engine and Unix socket, Docker Compose **2.30+** (`env_file.format: raw`), Python **3.9+**, OpenSSL **3+**.
- A manager image built from **this checkout's `Dockerfile`**, supplied explicitly as `MANAGER_IMAGE`. The sample never assumes a NAS-specific image is available; `pull_policy: never` makes a missing build fail visibly.
- Official Neko Chromium **3.2.0**, pinned by digest below. The generated owner image allowlist and sample room payload use the **same** `ROOM_IMAGE` input.
- Official Nginx **1.30.7-alpine** and coturn **4.6.3**, both pinned by digest in `compose.yaml`. Check the pinned Nginx binary's compiled stream, stream TLS and SSL-preread modules with `nginx -V`, then test the complete mounted config with `nginx -t` as below. Nginx and coturn are permissively licensed.

## 1. Choose an explicit, dedicated bridge and build the manager

From the repository root, inspect the **local machine** before choosing a subnet:

```sh
ip -4 route show
docker network ls
docker network inspect $(docker network ls -q) --format '{{.Name}} {{json .IPAM.Config}}'
```

Choose a new bridge CIDR that does not overlap existing Docker, host, LAN or VPN routes. Do not reuse a production network. The following values are **examples to check/change**, not auto-detected production settings. The dynamic pool excludes the static TURN address:

```sh
ROOM_NETWORK=neko-single-port-demo
ROOM_NETWORK_CIDR=172.30.83.0/24
TURN_PRIVATE_IP=172.30.83.10
ROOM_DYNAMIC_CIDR=172.30.83.128/25
# Only AFTER comparing the chosen CIDRs with the actual routes/networks above:
docker network create --driver bridge --subnet "$ROOM_NETWORK_CIDR" \
  --ip-range "$ROOM_DYNAMIC_CIDR" "$ROOM_NETWORK"

docker build -t neko-rooms-single-port:local .
docker pull ghcr.io/m1k1o/neko/chromium:3.2.0@sha256:270face8b8111e639744fb7bcdbbf0deed227900bb89d58b2f8adfc12af8c41e
```

`demo.py` **never creates or selects a network**. It requires the explicitly supplied bridge, validates its actual CIDR/gateway/address occupancy, and refuses mismatches. Coturn must share the room bridge: the browser's relayed traffic and the room's return traffic must reach each other's **private** IPs. This example does not provision split-host routes or tunnels.

## 2. Configure fresh private state, with a local development CA

No public domain or pre-existing certificate is needed. Configuration is explicit, local-only and starts no services:

```sh
mkdir -p "${XDG_CACHE_HOME:-$HOME/.cache}"
PRIVATE=$(mktemp -d "${XDG_CACHE_HOME:-$HOME/.cache}/neko-single-port.XXXXXX")
install -m 600 examples/single-port-relay/.env.example "$PRIVATE/settings.env"
# Edit this private file. Set MANAGER_IMAGE=neko-rooms-single-port:local and the
# exact ROOM_NETWORK, ROOM_NETWORK_CIDR, TURN_PRIVATE_IP chosen above.
# INSTANCE_NAME must be unique; do not run two states for the same instance.
"${EDITOR:-vi}" "$PRIVATE/settings.env"

python3 examples/single-port-relay/demo.py configure \
  --env-file "$PRIVATE/settings.env" --state-dir "$PRIVATE/state" --dev-ca
STATE="$PRIVATE/state"
```

Settings are **literal `KEY=value`**, not shell or dotenv: no interpolation, escapes, inline comments or removal of surrounding quotes. A dollar sign, `#`, backslash or quote in a supplied owner password is an actual character. Do not `source` the file. Unknown, duplicate, invalid and incomplete values fail instead of falling back. Only `APP_HOST`, `TURN_HOST`, **`PUBLIC_PORT`** exist for public addressing; there are no separate HTTPS/TURN public ports.

`--dev-ca` requires distinct **`.localhost`** names and a loopback bind. It rejects `--public`, a non-loopback bind, and supplied TLS paths. Defaults are `neko.localhost`, `turn.localhost`, `127.0.0.1`, port `8443`. Modern browsers resolve `.localhost` to loopback; if another client does not, configure both names locally or use that client's explicit resolution option. No DNS/hosts/trust store is changed automatically.

Generated state stays **outside the checkout** (directory `700`, files `600`):

| File/directory | Purpose |
| --- | --- |
| `compose.env` | Nonsecret Compose inputs and absolute paths; use with every Compose command |
| `manager.env` | Literal/raw owner credentials and authenticated frontend ICE JSON |
| `credentials.json` | Independent new Owner, native room user/admin and TURN credentials |
| `nginx.conf`, `turnserver.conf` | Complete gateway and private coturn configuration |
| `tls/ca.crt`, `tls/ca.key` | Local development CA; only import the **certificate**, never its key |
| `tls/server.crt`, `tls/server.key` | 30-day SAN server certificate covering **both** names |
| `room-request.json` | Optional native `api_version: 3` Owner API creation payload, name `native` |
| `storage/` | Fresh demo-only Profile/Downloads data; no existing installation path is used |

Repeated configure reuses credentials, CA, certificate and storage. It refuses implicit credential rotation, a changed SAN not covered by the existing certificate, partial trust material and an unrelated nonempty state directory. For a new CA/expired certificate, deliberately use a **new demo state/project**; do not silently replace a CA already trusted by a browser.

## 3. Trust the CA in your own browser, then start

Import **`$STATE/tls/ca.crt`** as a trusted website certificate authority in the browser you will use, then restart that browser. For Firefox use Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import. For Chromium use its certificate manager; on Linux an existing user NSS store can also be updated **explicitly**:

```sh
# Optional operator action, only if this is YOUR Chromium NSS database:
certutil -d "sql:$HOME/.pki/nssdb" -A -t 'C,,' \
  -n 'Neko single-port local CA' -i "$STATE/tls/ca.crt"
# Remove that explicit trust later with:
# certutil -d "sql:$HOME/.pki/nssdb" -D -n 'Neko single-port local CA'
```

An OS trust-store import is another **opt-in operator** choice, not performed by the script. Never use `--ignore-certificate-errors`, `curl -k`, or a browser certificate-warning bypass as the demo's trust setup: TURN/TLS also needs a trusted certificate. Keep the development CA key private and the deployment on loopback.

Use a direct Compose helper, without printing resolved secrets:

```sh
dc() { docker compose --env-file "$STATE/compose.env" \
  -f "$PWD/examples/single-port-relay/compose.yaml" "$@"; }
dc config --quiet
dc pull gateway turn
dc run --rm --no-deps gateway -V
dc run --rm --no-deps gateway -t
python3 examples/single-port-relay/demo.py up --state-dir "$STATE"
python3 examples/single-port-relay/demo.py status --state-dir "$STATE"

curl --fail --silent --show-error --cacert "$STATE/tls/ca.crt" \
  --resolve neko.localhost:8443:127.0.0.1 \
  https://neko.localhost:8443/manage/auth/status
```

Do not share `dc config`, `docker inspect` environment dumps, generated payloads or room logs: they can contain credentials. `config --quiet` validates without dumping resolved configuration. Pinned Neko logs frontend TURN credentials at info/debug; existing Manager relay policy defaults new rooms to `warn`.

## 4. Use the original Owner and native room interfaces

1. Open **`https://neko.localhost:8443/manage/`**. Privately read `credentials.json` in an editor and sign into the **existing Owner form** using `owner_username` / `owner_password`. No global Basic-auth or substitute login UI is added.
2. Create room **`native`**, API **3**, selecting the pinned allowed image. Enter the saved `room_user_password` / `room_admin_password` as its native user/admin passwords; keep the normal separate Profile and Downloads private mounts. `room-request.json` has those settings for clients using the **existing authenticated Owner API** (`POST /manage/api/rooms`, then the usual room-start API). Creation/start are not done by configure or `up`.
3. Use **`https://neko.localhost:8443/room/native/`** and log in with a display name plus a native room password. This path is proxied by Manager to the original Neko page, signaling and WebRTC. Owner cookies and Agent keys do not substitute for native viewer authentication. Native host control, keyboard/mouse, audio, clipboard and other room features remain Neko-owned.
4. Check browser WebRTC diagnostics for a selected **relay** candidate and advancing decoded video/audio. A certificate handshake, successful login or loaded HTML alone is **not media proof**. Separately exercise the native controls/features you need.

Manager stays `NEKO_ROOMS_NETWORK_MODE=relay`, not `http`. Its existing private/full-ICE policy and Neko 3.2 native-YAML startup adapter suppress public-IP discovery without changing the official room image. Only newly created/recreated rooms receive that policy. Do not point this sample at existing rooms/credentials/data.

Owner mutations must keep the exact HTTPS **Origin, including port**. Gateway forwarding preserves Host, HTTPS scheme, Authorization and WebSocket upgrades, so the existing Owner Secure-cookie and Agent Bearer boundaries remain in place. `/rooms/` never receives a gateway login challenge. **The stock official room image does not include this fork's CDP relay**: Agent authentication/routes remain unchanged, but actual CDP requires an explicitly selected compatible relay-enabled image; it is not supplied by this native TURN demo.

For a transport-only check of the mandatory no-SNI TURN branch (certificate verification is still enabled):

```sh
openssl s_client -connect 127.0.0.1:8443 -noservername \
  -CAfile "$STATE/tls/ca.crt" -verify_hostname turn.localhost \
  -verify_return_error </dev/null
```

TLS verification alone does not establish TURN authentication/allocation or WebRTC success. Runtime acceptance should also check TURN rejects missing/bad credentials, allocations advertise the private IP, public peer permissions fail, private-room peer permissions succeed, TCP relay requests fail, and every room has empty host port bindings.

## Public deployment uses the same one-port architecture

Supply **real distinct DNS names** (`APP_HOST`, `TURN_HOST`) pointing to the same gateway IP, `PUBLIC_PORT=443` (or your chosen single TCP port), the intended `PUBLIC_BIND_IP`, and absolute `TLS_CERT_FILE` / `TLS_KEY_FILE` paths to an unexpired fullchain/key covering **both names**. Run configure **without `--dev-ca`**; non-loopback binding additionally requires **`--public`**. Reserved test/example names are refused. Certificate issuance/renewal is external to this sample; DNS validation avoids needing port 80.

Forward only that TCP port at layer 4, with TLS passthrough. Do not put an HTTPS-only CDN, HTTP proxy or TLS-terminating ingress in front of TURN. Do not expose UDP, `5349`, `3478`, the allocation pool, Manager `8080`, CDP, or Docker. The selected public port appears in both HTTPS and `turns:TURN_HOST:PUBLIC_PORT?transport=tcp` URLs; `443` is canonically omitted only from the HTTPS origin.

Coturn uses normal private UDP allocations, long-term authentication, deny-all IPv4/IPv6 peer rules with a room-CIDR allowlist, `no-tcp-relay`, allocation quotas and bandwidth limits. Its allowlist trusts that **whole dedicated bridge**, not individual rooms. This is not a generic public UDP relay and needs no public UDP pool. TURN credentials are necessarily delivered to native room browsers; they are independent of Owner/Agent secrets.

## Tests and shutdown

```sh
python3 -B -m unittest discover -s examples/single-port-relay -p 'test_*.py' -v
# Stop/delete only the sample's chosen dynamic rooms via the Owner UI first.
python3 examples/single-port-relay/demo.py down --state-dir "$STATE"
```

The stdlib tests use temporary private files, a fake Unix socket and mocked Docker network inspection; when Compose is installed they also parse the **actual Compose file** without starting services. They check one TCP publication, identical public URI ports, explicit web SNI/default TURN, private upstreams, raw secret/path characters, native mode/image/payload, certificate SANs, state permissions and idempotency. They do **not** claim container/media/browser acceptance or change/pull/build any runtime image.

`down` removes only this Compose project's services. Manager-created rooms are **not Compose services** and are not automatically deleted; storage, private state and the explicitly supplied external network are retained. Remove only your own now-unused bridge and browser CA trust when finished. Restarting Manager invalidates its existing in-memory Owner sessions and Agent keys.

Mechanism references: [Nginx SSL preread](https://nginx.org/en/docs/stream/ngx_stream_ssl_preread_module.html) reads `$ssl_preread_server_name` without terminating TLS; [stream proxy_pass](https://nginx.org/en/docs/stream/ngx_stream_proxy_module.html#proxy_pass) resolves variable upstreams through the configured resolver; [coturn 4.6.3 configuration](https://github.com/coturn/coturn/blob/4.6.3/examples/etc/turnserver.conf) documents peer allow/deny precedence.
