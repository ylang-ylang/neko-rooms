# Native Neko, HTTPS + TURN/TLS on one TCP port

Open **`https://neko.localhost:8443/manage/`**, create a room, then use **`https://neko.localhost:8443/room/native/`**. For another device on your LAN, use the [direct-IP configuration below](#direct-lan-access-without-ssh). The viewer is the room image's **original Neko 3.2 UI and WebRTC**, including its native controls, audio, clipboard and other features. No copied frontend, MSE player, `aggregate.html`, or Manager `http` mode is used.

This is a **fresh, local, single-Docker-host example** in the Manager project. Its [room deployment adapter](room-cdp/) derives from the pinned official Neko image to enable CDP on the **same headed Chromium** and add a private relay. Neko's core binary, frontend, native controls and WebRTC stay upstream; the deployment image is **not byte-identical** to the official image. No existing installation or NAS is modified. [The two-port example](../relay/) remains separate.

```text
one published TCP socket: <chosen-bind>:8443 -> gateway:443
                                   TLS ClientHello preread (no termination)
    HTTP ALPN h2 / http/1.1 -------> 127.0.0.1:8443 HTTPS -> neko-rooms:8080
    distinct web SNI, no ALPN -----> same HTTPS fast path
    other / unmatched TLS --------> 127.0.0.1:5349 TLS termination
                                         -> classifier:3479 (HAProxy TCP)
                                             HTTP -> gateway:8082 -> neko-rooms:8080
                                             STUN -> turn:3478 TCP
                                             unknown -> reject
    plain HTTP -------------------> 127.0.0.1:8081 HTTPS redirect / LAN-dev CA
    private UDP allocations: turn <-> native rooms
```

The loopback addresses above are **inside the gateway container**, not additional host bindings. Only its `443/tcp` is published. HAProxy, coturn, Manager and Manager-created relay rooms publish no host ports; the UDP allocation pool is private. Docker DNS is re-resolved after container recreation. HAProxy uses `init-addr none` so its private gateway backend can appear after the classifier starts, without a startup dependency cycle or fixed container IP.

Modern browsers advertising **HTTP ALPN** (`h2` / `http/1.1`) select the existing HTTPS fast path, as does distinct application-name SNI when ALPN is absent. Other TLS connections, including **no SNI and no ALPN**, use the same Nginx certificate and TLS termination before HAProxy inspects the decrypted first request. HAProxy's built-in `req.proto_http` parser selects HTTP; a complete 20-byte STUN header with cookie `2112a442` selects coturn. Unknown or incomplete traffic is explicitly rejected within **5 seconds**, never silently sent to TURN. After positive classification the connection is transparent, including HTTP keepalive, WebSocket upgrades and subsequent TURN ChannelData. HAProxy has no TLS certificates or keys. The private cleartext web backend still forwards **HTTPS** scheme and the original public Host/port to Manager; the public plaintext branch remains certificate-only bootstrap plus redirect.

Ordinary HTTPS/WSS clients such as Node HTTPS, `ws` and Playwright can therefore use the canonical IP URL without custom ALPN or SNI. The web URL can remain a LAN IP, but **Firefox TURN/TLS requires a FQDN**, as described below. Public ingress must forward **raw TCP at layer 4**, preserving ClientHello; an ordinary HTTP reverse proxy cannot carry TURN.

## Requirements

- Local Linux Docker Engine and Unix socket, Docker Compose **2.30+** (`env_file.format: raw`), Python **3.9+**, OpenSSL **3+**.
- A manager image built from **this checkout's `Dockerfile`**, supplied explicitly as `MANAGER_IMAGE`. The sample never assumes a NAS-specific image is available; `pull_policy: never` makes a missing build fail visibly.
- The local **CDP-enabled room deployment image**, built below from official Neko Chromium **3.2.0** pinned by digest. The generated owner image allowlist and sample room payload use the **same** `ROOM_IMAGE` input, including when you choose a custom build tag.
- Official Nginx **1.30.7-alpine**, HAProxy LTS **3.2-alpine** and coturn **4.6.3**, pinned by pulled repository digests in `compose.yaml`. Check the pinned Nginx binary's compiled stream, stream TLS and SSL-preread modules with `nginx -V`, then validate Nginx and HAProxy configs as below. HAProxy is GPLv2; Nginx and coturn are permissively licensed.

## 1. Choose an explicit, dedicated bridge and build both images

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
docker build -t neko-single-port-chromium-cdp:3.2.0-1 examples/single-port-relay/room-cdp
```

Builds must complete on the **same Docker daemon** Manager will use. Keep `ROOM_IMAGE=neko-single-port-chromium-cdp:3.2.0-1` in the private settings below; if you build another tag, set that exact tag there before configure. The stock upstream image alone has neither CDP flags nor the relay. Manager does not install them or pull missing room images.

`demo.py` **never creates or selects a network**. It requires the explicitly supplied bridge, validates its actual CIDR/gateway/address occupancy, and refuses mismatches. Coturn must share the room bridge: the browser's relayed traffic and the room's return traffic must reach each other's **private** IPs. This example does not provision split-host routes or tunnels.

## 2. Configure fresh private state, with a local development CA

No public domain or pre-existing certificate is needed. The default setup binds to loopback; explicit LAN mode is described below. Configure generates private files and starts no services:

```sh
mkdir -p "${XDG_CACHE_HOME:-$HOME/.cache}"
PRIVATE=$(mktemp -d "${XDG_CACHE_HOME:-$HOME/.cache}/neko-single-port.XXXXXX")
install -m 600 examples/single-port-relay/.env.example "$PRIVATE/settings.env"
# Edit this private file. Set MANAGER_IMAGE=neko-rooms-single-port:local,
# ROOM_IMAGE to the exact built CDP adapter tag, and the
# exact ROOM_NETWORK, ROOM_NETWORK_CIDR, TURN_PRIVATE_IP chosen above.
# INSTANCE_NAME must be unique; do not run two states for the same instance.
"${EDITOR:-vi}" "$PRIVATE/settings.env"

python3 examples/single-port-relay/demo.py configure \
  --env-file "$PRIVATE/settings.env" --state-dir "$PRIVATE/state" --dev-ca
STATE="$PRIVATE/state"
```

Settings are **literal `KEY=value`**, not shell or dotenv: no interpolation, escapes, inline comments or removal of surrounding quotes. A dollar sign, `#`, backslash or quote in a supplied owner password is an actual character. Do not `source` the file. Unknown, duplicate, invalid and incomplete values fail instead of falling back. DNS mode uses `APP_HOST`, `TURN_HOST` and **`PUBLIC_PORT`**; LAN mode uses `--lan-ip` for HTTPS and optional `--turn-host` for TURN. There are no separate HTTPS/TURN public ports.

Without `--lan-ip`, `--dev-ca` requires **`.localhost`** names and a loopback bind, without `--public`. Defaults remain `neko.localhost`, `turn.localhost`, `127.0.0.1`, port `8443`. Modern browsers resolve `.localhost` to loopback; if another client does not, use that client's explicit resolution option. Development CA mode never accepts supplied TLS paths. No DNS/hosts/trust store is changed automatically.

Generated state stays **outside the checkout** (state directory `700`, private files `600`; the nonsecret HAProxy config is `644` for its unprivileged container user, and LAN mode additionally makes a public certificate-only copy):

| File/directory | Purpose |
| --- | --- |
| `compose.env` | Nonsecret Compose inputs and absolute paths, including `HAPROXY_CONFIG_FILE`; use with every Compose command |
| `manager.env` | Literal/raw owner credentials and authenticated frontend ICE JSON |
| `credentials.json` | Independent new Owner, native room user/admin and TURN credentials |
| `nginx.conf`, `turnserver.conf` | Complete gateway and private coturn configuration |
| `haproxy.cfg` | Private-bridge HTTP/STUN classifier, no secrets/keys; single-file read-only mount (`644`) |
| `tls/ca.crt`, `tls/ca.key` | Local development CA; only import the **certificate**, never its key |
| `tls/server.crt`, `tls/server.key` | 30-day server certificate covering the configured **IP and/or DNS SANs** |
| `public/ca.crt` | LAN-dev-only public CA certificate copy (`644`, directory `755`), mounted read-only; never a key or credentials |
| `room-request.json` | Optional native `api_version: 3` Owner API creation payload, name `native` |
| `storage/` | Fresh demo-only Profile/Downloads data; no existing installation path is used |

Repeated configure reuses credentials, CA, certificate and storage. It refuses implicit credential rotation, a changed SAN not covered by the existing certificate, partial trust material and an unrelated nonempty state directory. To change SANs or renew an expired development leaf, explicitly add **`--renew-server-cert`**: only `server.crt` is replaced, retaining the existing CA, server key, credentials and storage. Replacing the CA itself requires a deliberately new demo state/project.

### Direct LAN access without SSH

**IP HTTPS and TURN/TLS have different client requirements.** Firefox 155 on Linux rejects IP-literal `turns:` URLs with `TLS requires FQDN, not IP address` before the TLS handshake, even after CA import. Keep the web URL as an IP, but configure a **TURN FQDN** for Firefox. Omitting `--turn-host` retains the old IP TURN mode only for clients that support it; IP HTTPS success is not proof of IP TURN compatibility.

For example, if the Docker host's LAN address is `192.168.201.237`, set **`PUBLIC_BIND_IP=0.0.0.0`** (or that LAN IP) and **`PUBLIC_PORT=8443`** in your private settings. Choose a hostname that the **client's DNS resolves to that LAN IP**, then configure:

```sh
TURN_FQDN=debian-vm-2.lan # Example LAN DNS name; use your client-resolvable FQDN.
python3 examples/single-port-relay/demo.py configure \
  --env-file "$PRIVATE/settings.env" --state-dir "$STATE" \
  --lan-ip 192.168.201.237 --turn-host "$TURN_FQDN" --dev-ca --public
```

`--turn-host` is accepted only with explicit LAN mode (`--lan-ip --dev-ca --public`), must be a non-IP, non-`.localhost` FQDN, and is not inherited from settings `TURN_HOST`. Configure validates syntax but never changes DNS/hosts or verifies client DNS resolution. The leaf covers **IP:192.168.201.237 plus DNS:TURN_FQDN**.

When adding the hostname to an existing IP-only or `.localhost` state, back up its server certificate and add **`--renew-server-cert` once**: only the leaf changes, preserving the CA, server key, credentials and storage. Keep both LAN identity flags on subsequent configure runs; omit renewal unless needed. Browsers already trusting that CA need no re-import; a new client must still import it once. For an already running demo, use `dc up -d --force-recreate` with the section 3 helper to reload generated mounts/config, then recreate only your sample room to receive the new frontend ICE URL.

Start the services using section 3, then use these URLs directly from the LAN client:

- Download the **public CA certificate only**: http://192.168.201.237:8443/ca.crt — import it as a trusted website CA before using HTTPS/TURN.
- Owner UI: https://192.168.201.237:8443/manage/
- Native viewer, after creating room `native`: https://192.168.201.237:8443/room/native/
- Generated native ICE URL for the example hostname: `turns:debian-vm-2.lan:8443?transport=tcp`.

No SSH tunnel, hosts-file edit or extra host port is needed; the chosen TURN hostname must resolve correctly on the client. The same **TCP 8443** serves the LAN-dev certificate download; other HTTP paths return **308** to HTTPS at the same authority. Owner/viewer traffic and native TURN remain HTTPS/TLS with certificate verification. The download alias exists only in explicit LAN-dev mode and exposes neither private keys nor credentials.

**Non-browser HTTPS/WSS clients:** use the IP HTTPS/CDP URL normally, with the development CA trusted. No explicit ALPN, custom SNI, hosts-file entry or certificate-verification bypass is needed. Upgrading an existing state only needs the same `configure` command (keep its existing LAN/hostname flags, **without** `--renew-server-cert`), then `dc up -d --no-deps --force-recreate classifier gateway` to start the private classifier and recreate the gateway; do not restart Manager or native rooms for this ingress-only change. Credentials, certificates, coturn realm and allocations remain unchanged.

## 3. Start services and trust the CA in your browser

Import **`$STATE/tls/ca.crt`** as a trusted website certificate authority in the browser you will use, then restart that browser. For a separate LAN device, start the services below first and obtain this certificate through the LAN download URL above. For Firefox use Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import. For Chromium use its certificate manager; on Linux an existing user NSS store can also be updated **explicitly**:

```sh
# Optional operator action, only if this is YOUR Chromium NSS database:
certutil -d "sql:$HOME/.pki/nssdb" -A -t 'C,,' \
  -n 'Neko single-port local CA' -i "$STATE/tls/ca.crt"
# Remove that explicit trust later with:
# certutil -d "sql:$HOME/.pki/nssdb" -D -n 'Neko single-port local CA'
```

An OS trust-store import is another **opt-in operator** choice, not performed by the script. Never use `--ignore-certificate-errors`, `curl -k`, or a browser certificate-warning bypass as the demo's trust setup: TURN/TLS also needs a trusted certificate. Keep the development CA key private; non-loopback development access requires the explicit LAN configuration above.

Use a direct Compose helper, without printing resolved secrets:

```sh
dc() { docker compose --env-file "$STATE/compose.env" \
  -f "$PWD/examples/single-port-relay/compose.yaml" "$@"; }
dc config --quiet
dc pull gateway classifier turn
dc run --rm --no-deps gateway -V
dc run --rm --no-deps gateway -t
dc run --rm --no-deps classifier -c -f /usr/local/etc/haproxy/haproxy.cfg
python3 examples/single-port-relay/demo.py up --state-dir "$STATE"
python3 examples/single-port-relay/demo.py status --state-dir "$STATE"

curl --fail --silent --show-error --cacert "$STATE/tls/ca.crt" \
  --resolve neko.localhost:8443:127.0.0.1 \
  https://neko.localhost:8443/manage/auth/status
```

The `curl` example above is for loopback mode. In LAN mode, omit `--resolve` and use **`https://192.168.201.237:8443/manage/auth/status`** with the same `--cacert` option.

Do not share `dc config`, `docker inspect` environment dumps, generated payloads or room logs: they can contain credentials. `config --quiet` validates without dumping resolved configuration. Pinned Neko logs frontend TURN credentials at info/debug; existing Manager relay policy defaults new rooms to `warn`.

## 4. Use the original Owner and native room interfaces

The steps below show loopback URLs; in LAN mode, use the IP URLs above instead.

1. Open **`https://neko.localhost:8443/manage/`**. Privately read `credentials.json` in an editor and sign into the **existing Owner form** using `owner_username` / `owner_password`. No global Basic-auth or substitute login UI is added.
2. Create room **`native`**, API **3**, selecting the built CDP adapter image. Enter the saved `room_user_password` / `room_admin_password` as its native user/admin passwords; keep the normal separate Profile and Downloads private mounts. `room-request.json` has those settings for clients using the **existing authenticated Owner API** (`POST /manage/api/rooms`, then the usual room-start API). Creation/start are not done by configure or `up`.
3. Use **`https://neko.localhost:8443/room/native/`** and log in with a display name plus a native room password. This path is proxied by Manager to the original Neko page, signaling and WebRTC. Owner cookies and Agent keys do not substitute for native viewer authentication. Native host control, keyboard/mouse, audio, clipboard and other room features remain Neko-owned.
4. Check browser WebRTC diagnostics for a selected **relay** candidate and advancing decoded video/audio. A certificate handshake, successful login or loaded HTML alone is **not media proof**. Separately exercise the native controls/features you need.

Manager stays `NEKO_ROOMS_NETWORK_MODE=relay`, not `http`. Its existing private/full-ICE policy and Neko 3.2 native-YAML startup adapter suppress public-IP discovery; the room deployment adapter leaves this startup mechanism intact. Only newly created/recreated rooms receive those settings. Do not point this sample at existing rooms/credentials/data.

Owner mutations must keep the exact HTTPS **Origin, including port**. Gateway forwarding preserves Host, HTTPS scheme, Authorization and WebSocket upgrades, so the existing Owner Secure-cookie and Agent Bearer boundaries remain in place. `/rooms/` never receives a gateway login challenge.

### Agent CDP uses the same native browser

Use the Owner room list's **Agent** action to issue a temporary, room-name-bound key. Use its generated client snippet and CDP URL with **`Authorization: Bearer …`**, through the same gateway HTTPS port (`/rooms/<name>/json/version`, `/rooms/<name>/devtools/…`). Owner cookies and native room passwords do not authenticate CDP. Reuse the existing browser context and tabs; do not launch another browser, create another profile or call `Browser.close` when disconnecting. Neko control ownership does not serialize CDP input: coordinate human/agent handoff explicitly.

The adapter makes only these deployment changes:

- Preserve the original supervisor Chromium/Openbox launch and all its other flags; set that **one headed process's** profile to the sample's existing private mount **`/home/neko/profile`**, and add `--remote-debugging-address=127.0.0.1 --remote-debugging-port=9222`. The official launcher uses `/home/neko/.config/chromium`; existing data there is **not migrated** by configure or an image rebuild. Recreate only an intended sample room; a changed image does not enable CDP in an already-running browser.
- Enable `DeveloperToolsAvailability` and `RemoteDebuggingAllowed` in the existing managed policy, preserving unrelated policies. Upstream core, UI, healthcheck and image command remain unchanged.
- Supervise the packaged **socat** relay on **9223 inside the room's Docker network** to loopback 9222. Neither port is host-published. No `--remote-allow-origins=*` or alternate viewer is added. The relay preserves request bytes, including Chromium's Host/Origin checks. Manager authenticates the external Bearer connection, supplies loopback Host and creates its own upstream handshake without forwarding the external Origin or credentials.

The private bridge is trusted: unauthenticated CDP is reachable by its members on 9223, so attach only this dedicated deployment's trusted services/rooms. Internet clients must use Manager's name-bound temporary key, never direct CDP, a published relay, or a room password. Manager's existing key expiry/revocation closes active sockets and cross-room keys are rejected; changing the room image does not replace that gate.

For a TLS-only check of the no-SNI/no-ALPN branch in loopback mode (certificate verification is still enabled; without application data the classifier rejects after its inspection deadline):

```sh
openssl s_client -connect 127.0.0.1:8443 -noservername \
  -CAfile "$STATE/tls/ca.crt" -verify_hostname turn.localhost \
  -verify_return_error </dev/null
```

In LAN mode, use `-connect 192.168.201.237:8443` and replace `-verify_hostname turn.localhost` with `-verify_hostname "$TURN_FQDN"`. Use `-verify_ip 192.168.201.237` only for the optional legacy IP TURN mode.

TLS verification alone does not establish TURN authentication/allocation or WebRTC success. Runtime acceptance should also check TURN rejects missing/bad credentials, allocations advertise the private IP, public peer permissions fail, private-room peer permissions succeed, TCP relay requests fail, and every room has empty host port bindings.

## Public deployment uses the same one-port architecture

For a DNS-based production setup, **one real FQDN is sufficient**: set `APP_HOST` and `TURN_HOST` to the **same name** resolving to the gateway IP. Distinct web/TURN names pointing to that IP remain optional. Set `PUBLIC_PORT=443` (or your chosen single TCP port), the intended `PUBLIC_BIND_IP`, and absolute `TLS_CERT_FILE` / `TLS_KEY_FILE` paths to an unexpired fullchain/key covering **all configured names** (only one DNS name when shared). Shared-name clients without ALPN use the post-TLS HTTP/STUN classifier; no second hostname is needed to distinguish HTTPS from TURN/TLS. Run configure **without `--dev-ca`**; non-loopback binding additionally requires **`--public`**. Reserved test/example names are refused. Certificate issuance/renewal is external to this sample; DNS validation avoids needing port 80.

Forward only that TCP port at layer 4, with TLS passthrough. Do not put an HTTPS-only CDN, HTTP proxy or TLS-terminating ingress in front of TURN. Do not expose UDP, `5349`, `3478`, the allocation pool, Manager `8080`, CDP, or Docker. The selected public port appears in both HTTPS and `turns:TURN_HOST:PUBLIC_PORT?transport=tcp` URLs; `443` is canonically omitted only from the HTTPS origin.

Coturn uses normal private UDP allocations, long-term authentication, deny-all IPv4/IPv6 peer rules with a room-CIDR allowlist, `no-tcp-relay`, allocation quotas and bandwidth limits. Its allowlist trusts that **whole dedicated bridge**, not individual rooms. This is not a generic public UDP relay and needs no public UDP pool. TURN credentials are necessarily delivered to native room browsers; they are independent of Owner/Agent secrets.

## Tests and shutdown

```sh
python3 -B -m unittest discover -s examples/single-port-relay -p 'test_*.py' -v
# Stop/delete only the sample's chosen dynamic rooms via the Owner UI first.
python3 examples/single-port-relay/demo.py down --state-dir "$STATE"
```

The stdlib tests use temporary private files, a fake Unix socket and mocked Docker network inspection; when Compose is installed they also parse the **actual Compose file** without starting services. They check one TCP publication, identical public URI ports, HTTP ALPN/legacy SNI fast paths, post-TLS HTTP/STUN classification with explicit unknown rejection, private HTTPS forwarding headers, explicit LAN/hostname flags, private upstreams, raw secret/path characters, native mode/image/payload, mixed DNS/IP SAN validation, leaf-only renewal, certificate-only bootstrap, state permissions and idempotency. Additional adapter tests check preservation of native Chromium argv and unrelated policies, loopback CDP, supervised byte-preserving relay, no added entrypoint/host publication, and custom room-image equality between the creation payload and Manager allowlist. The default run does **not** claim container/media/browser acceptance or change/pull/build any runtime image.

For real-container ingress regression tests, first build the example room image and pull the three pinned ingress images using the commands above, then opt in:

```sh
NEKO_RELAY_DOCKER_TESTS=1 python3 -B -m unittest discover \
  -s examples/single-port-relay -p test_ingress.py -v
```

This suite creates its **own** temporary bridge, CA, credentials and one random **loopback-only** TCP publication; it never reads an existing deployment's state or attaches to its network. It runs the actual HAProxy binary/config and production gateway/coturn definitions, replacing only Manager with a transport/header fixture. It checks strict CA-trusted default Node IP HTTPS without ALPN/SNI, the ALPN fast path, preserved HTTPS Host/Origin/Bearer headers, authenticated fixture WebSocket upgrades, fragmented STUN authentication/allocation and subsequent TURN ChannelData echo, explicit unknown/incomplete rejection, and certificate-only plaintext bootstrap/redirect. The same checks also run with `APP_HOST=TURN_HOST=shared.neko.localhost`, one DNS SAN and ordinary SNI without ALPN; test-local lookup maps only that fixture name to loopback without changing system/public DNS. Resources are removed after the run; no images are pulled. Native Neko control/media and stock Playwright CDP still require separate end-to-end validation against real rooms.

`down` removes only this Compose project's services. Manager-created rooms are **not Compose services** and are not automatically deleted; storage, private state and the explicitly supplied external network are retained. Remove only your own now-unused bridge and browser CA trust when finished. Restarting Manager invalidates its existing in-memory Owner sessions and Agent keys.

Mechanism references: [HAProxy 3.2 configuration manual](https://docs.haproxy.org/3.2/configuration.html) documents `tcp-request inspect-delay`, `req.proto_http`, `req.len`, `req.payload`, `WAIT_END` and Docker-style runtime `resolvers` / `init-addr none`; [Nginx SSL preread](https://nginx.org/en/docs/stream/ngx_stream_ssl_preread_module.html) reads `$ssl_preread_alpn_protocols`, `$ssl_preread_server_name` and `$ssl_preread_protocol` without terminating TLS; [stream proxy_pass](https://nginx.org/en/docs/stream/ngx_stream_proxy_module.html#proxy_pass) resolves variable upstreams through the configured resolver; [coturn 4.6.3 configuration](https://github.com/coturn/coturn/blob/4.6.3/examples/etc/turnserver.conf) documents peer allow/deny precedence.
