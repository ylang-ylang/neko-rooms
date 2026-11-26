# TrueNAS: native Neko on one TCP port

## Default: plain HTTP, WebSocket and TURN/TCP

The default entry is **http://192.168.202.11:22222/manage/** with the existing `rooms.env` Owner credentials. Native room pages use `/room/NAME/`; room-bound Agent CDP uses `/rooms/NAME/`. The browser receives **`turn:192.168.202.11:22222?transport=tcp`**. No TLS, CA generation, certificate import or Node CA setting is needed in this mode. `NEKO_ROOMS_NETWORK_MODE` remains **relay**: this is the original Neko WebRTC UI, not the historical `http` aggregate/MSE viewer.

Nginx forwards public TCP22222 to the existing HAProxy classifier. It recognizes an HTTP request or the STUN magic cookie, then forwards the entire connection unchanged to private HTTP or coturn. WebSocket upgrades and subsequent TURN ChannelData are not reclassified. Unknown/incomplete traffic is rejected, not silently treated as TURN. Browser-to-TURN uses TCP; TURN-to-room media remains private bridge UDP. Manager, classifier, coturn and relay rooms publish no additional ports.

Existing room containers need not be recreated when changing entry transport. Their native viewer receives a small transport-only adapter before the upstream client starts: the matching old TURN URL is replaced in `RTCPeerConnection` construction/configuration, preserving credentials and every other ICE option. New rooms receive the selected URL directly in Neko configuration. Native signaling, video, controls and the headed browser are unchanged. The adapter also supports an explicitly selected TLS rollback.

Default legacy services retain28320/28321 and UDP52100–52110. Those ports are not the unified entry. Existing external room ports/modes/data are not implicitly migrated. The authorized NAS is **192.168.202.11**; workstation `truenas.lan`/`truenas` resolves to a different host at192.168.201.11.

## Source ownership and configuration

Tooling belongs here in **neko-rooms**. The sole manifest source is the independent **truenas-apps** repository's root `neko.compose.yaml`. Do not copy tooling into that repository, create another production Compose project, or paste a template with unresolved `${...}` into TN.

From the source checkout, prepare private settings without replacing existing authentication or data:

```sh
app=/mnt/ylangsPool/appdata/neko
manifest=../truenas-apps/neko.compose.yaml
test -f "$manifest"
test -e "$app/native-relay.settings" || install -m 600 deploy/truenas/settings.example "$app/native-relay.settings"
docker network inspect neko-rooms-net --format '{{json .IPAM.Config}}'
```

Set `TRANSPORT=plain`, `APP_HOST=192.168.202.11`, `PUBLIC_PORT=22222`, the intended `PUBLIC_BIND_IP`, and the actual existing `ROOM_NETWORK_CIDR`. Blank `TURN_HOST` inherits `APP_HOST`. Keep `TURN_PRIVATE_IP=auto`: Docker assigns the private address and HAProxy resolves `turn:3478`. An inspect-reported subnet does not imply that Docker permits static endpoint IPs; never recreate the bridge carrying user rooms to enable static addressing.

Leave both TLS paths blank in plain mode. Existing TLS/CA files remain untouched for rollback. Permanent Owner/viewer credentials stay in existing files, and saved TURN credentials are reused without rotation.

```sh
python3 deploy/truenas/configure.py --compose-template "$manifest" \
  --settings "$app/native-relay.settings" --public
```

Generation writes only owned `native-relay` state; it does not start services or move profiles. The public `room-ice.js` contains URLs only, never credentials. Root YAML has an explicit TLS-only mount block which the renderer omits in plain mode. Paste the complete generated `native-relay/neko.compose.yaml` into the existing TN App, not the parameterized source. Env files retain their original Compose semantics; generation does not expand permanent passwords. TN bundles Python `yaml` for conversion without env expansion:

```sh
python3 - "$app/native-relay/neko.compose.yaml" "$app/native-relay/neko.compose.json" <<'PY'
import json, os, pathlib, sys, yaml
os.umask(0o077)
pathlib.Path(sys.argv[2]).write_text(json.dumps(yaml.safe_load(pathlib.Path(sys.argv[1]).read_text())))
PY
```

## Updates and interruption scope

The cached publisher is `python3 scripts/release.py publish`. It builds committed inputs, reuses immutable images and does not rebuild/upload unchanged Browser content. The usual wrapper is:

```sh
bash deploy/truenas/publish-to-tn.sh
```

Changing only the Manager image **in configuration does not mean Manager-only runtime restart**. TN25.04 uses `compose up --force-recreate --remove-orphans`, recreating all App services, including the default browser and CDP proxy. External Manager-created room containers remain outside that project. Obtain approval for that interruption, preserve the current App/config/container/process/mount/auth state and whole opaque unmounted default NSS directory first. Never overwrite a live NSS database; restore only into the same stopped container and start that container, not an App-level start which may recreate it again. Mounted data/auth must not be reset.

An approved topology/transport change uses the same wrapper:

```sh
bash deploy/truenas/publish-to-tn.sh \
  --compose /mnt/ylangsPool/appdata/neko/native-relay/neko.compose.json \
  --baseline /mnt/ylangsPool/appdata/neko/backups/YOUR_SNAPSHOT
```

These are NAS paths. The updater preserves current default definitions/configs exactly, pins release images, pre-pulls, validates and issues **one** official `app.update`. It compares external IDs/start/init/Chromium PIDs/mounts, default image/ports/mounts and auth/config/data-root metadata. Errors stop without hidden rollback. `TN_SSH_CONTROL_PATH` reuses an existing persistent SSH transport. The networkless `room-image` service runs only `/bin/true` and exits0; it is an image-delivery prerequisite, not a crashed browser.

Manager restart invalidates RAM Owner sessions and Agent keys: sign in and issue keys again. HTTP and HTTPS Owner cookies use distinct names so an old Secure cookie cannot block explicit HTTP login. Native room passwords remain separate from Owner credentials and Agent Bearer keys. Do not call `Browser.close` on a shared room browser.

## Optional TLS mode

TLS is opt-in: set `TRANSPORT=tls`. Supply an existing private `TLS_CERT_FILE`/`TLS_KEY_FILE` covering `APP_HOST` and `TURN_HOST`, or explicitly use `--dev-ca` with both paths blank. Private-CA clients then require manual public-CA trust; only this optional mode uses HTTPS/WSS and `turns:`. IP SANs work in Chromium, but Firefox rejects numeric TURNS with `TLS requires FQDN, not IP address`; use a client-resolvable DNS TURN host for Firefox TLS mode. Do not change client DNS/hosts or import into user trust stores automatically.

Existing CA/keys/TURN credentials are retained. `--renew-server-cert --dev-ca` replaces only the generated leaf. Saved TLS configurations and historical acceptance remain rollback evidence, not the default plaintext endpoint. The separate `.237` development example remains independent and is not changed by TN configuration.

## Profiles, verification and rollback

Profiles are durable independent resources. Removing a deployment keeps its Profile/Downloads by default; permanent deletion is an unchecked, explicit separate choice. Existing managed directories are adopted in place. Mounted Chromium files already persist localStorage/LevelDB and other identity. Optional buffered checkpoint replay failure must **warn and keep a healthy deployment running**, retaining the checkpoint; live clones still require their complete supplemental replay. See [storage](../../docs/storage.md).

Read-only endpoint checks use the configured transport:

```sh
python3 deploy/truenas/configure.py --settings "$app/native-relay.settings" --check-endpoint
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/truenas -p 'test_*.py' -v
node --test deploy/truenas/test_room_ice.cjs
```

Endpoint checks do not establish media. Full acceptance needs real Chromium and Firefox: login, native1920×1080 continuous rendered frames, selected client `relay` with `relayProtocol=tcp` in plain mode, normal ICE `all`, and same-port stock Playwright CDP plus native input on an owned canary. Test with a real IP and `isSecureContext=false`, not a localhost exception or certificate bypass. Remove only owned canary resources.

Before transport changes, privately save the current App definition, settings and matching owned relay files, including the ICE adapter, plus opaque NSS and preservation evidence. To roll back, restore the matching owned relay/settings set and call official `app.update` with that snapshot's `app-config.json`; update the host mirror only after success. Repeat preservation checks. Do not overwrite live Profile/download/auth data for configuration rollback. Historical snapshots deliberately retain their original HTTP28322, HTTPS28322 or HTTPS22222 endpoints. Current plain snapshots use HTTP22222; do not rewrite historical evidence to look current.
