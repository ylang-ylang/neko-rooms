# TrueNAS: publishing and the native single-TCP endpoint

## Routine release: one command, existing topology

This directory belongs to the **neko-rooms source repository**. The independent **truenas-apps** repository accepts root App YAML only, not scripts, templates, tests or source code. This directory does not keep another `neko.compose.yaml`.

Run from the **neko-rooms repository root**:

```sh
bash deploy/truenas/publish-to-tn.sh
```

This wrapper invokes the existing `python3 scripts/release.py publish` incremental/cached publisher, transfers `release.json`, and uses the existing SSH key to invoke TN's `update.py --release ...`. It changes only the Manager image **in the configuration** and pre-pulls the Browser image; this does **not** imply only the Manager container restarts. The installed TN 25.04 middleware runs `compose up --force-recreate --remove-orphans` for a RUNNING App update, recreating all App services, including the default `neko` browser and `cdp-proxy`, even when their definitions and images are unchanged. Manager-created dynamic rooms outside the `ix-neko` Compose project are not those App services.

**Do not run the update when the default browser must remain uninterrupted.** Snapshot the current App definition/container state and obtain approval for App-service recreation first. The helper backs up configuration privately. Failures or preservation mismatches stop without automatically issuing a second update; recovery is an operator decision, and official-API restoration also recreates App services. Permanent env/auth files and bind-mounted data are not reset, but browser processes and their in-memory sessions are interrupted.

**TN currently retains its original direct topology at `http://192.168.202.11:28322/manage/`, using Owner credentials from the existing `rooms.env`.** Initial single-port migration is separate from routine image updates. The authorized NAS is `.202.11`; `truenas.lan` resolves to a different host at `.201.11` and must not be used for this migration. The single-port instructions below accept a DNS name or literal IP, including `APP_HOST=192.168.202.11` with blank `TURN_HOST`. DNS is not a migration prerequisite. Chromium supports IP TURN/TLS with a trusted IP SAN; Firefox rejects numeric `turns:` URLs before TLS, but IP HTTPS and direct ICE remain usable.

To apply an already published Manager on TN itself:

```sh
sudo -n python3 /home/truenas_admin/neko-native-release/update.py --manager-image docker.io/ylangylang/neko-rooms:single-port
```

Durable browser profiles live under the existing Manager storage mount. Upgrades register legacy managed directories in place without restarting browsers. Deployment removal keeps the profile and Downloads by default; select that profile when creating a replacement. The removal confirmation offers an **unchecked** option to permanently delete only its Profile after the deployment is removed. The separate **Profiles** action also deletes available profiles only. See [storage ownership and recovery](../../docs/storage.md).

## Initial native single-port deployment (DNS or IP)

The sole manifest source remains the independent **truenas-apps repository's root [`neko.compose.yaml`](https://github.com/ylang-ylang/truenas-apps/blob/main/neko.compose.yaml)**. Do not create another App or move it into a `neko/` subdirectory. The implementation comes from `examples/single-port-relay/`; its three proxy templates are preserved. Native Neko 3.2 UI, core and Chromium are not replaced with aggregate/MSE pages.

- **28322/tcp** upgrades the old Manager HTTP endpoint to same-port **HTTPS/WSS + TURN/TLS + Agent CDP**. Nginx retains the HTTP ALPN/SNI fast path; without SNI/ALPN, HAProxy distinguishes HTTP/STUN after TLS termination. Ordinary Playwright requires no special transport parameters.
- Manager, HAProxy, coturn and new relay rooms publish no additional HostPorts. TURN UDP allocations remain inside the existing `neko-rooms-net`. An ordinary HTTP reverse proxy cannot replace this endpoint.
- The old default room's `28320/28321`, `52100-52110/udp`, `profile/`, `downloads/`, `config/` and authentication files remain in place. These old ports are not the new public endpoint.
- Legacy `rooms-data/rooms/NAME/{profile,downloads}` directories are registered as durable profile roots in place. New profiles use independent IDs, not deployment names. Old test data is not deleted automatically.

## 1. Prepare once; TN pulls images directly

Keep separate `neko-rooms` and `truenas-apps` checkouts on TN. Run these commands from the **neko-rooms root**. Python 3, OpenSSL and TN's bundled Compose **2.32.3** suffice; no Python packages or local image builds are required. The default manifest path is adjacent `../truenas-apps/neko.compose.yaml`. Supply an explicit path when the checkouts are not adjacent; do not copy scripts into the manifest repository.

```sh
app=/mnt/ylangsPool/appdata/neko
manifest=../truenas-apps/neko.compose.yaml  # Use an absolute path if not adjacent.
test -f "$manifest"
test -e "$app/native-relay.settings" || install -m 600 deploy/truenas/settings.example "$app/native-relay.settings"
docker network inspect neko-rooms-net --format '{{json .IPAM.Config}}'
```

Fill in the private `native-relay.settings`:

1. `APP_HOST`: a reachable DNS name or literal IP. For TN IP mode use `192.168.202.11`; blank `TURN_HOST` reuses it. Chromium IP TURN/TLS was verified with strict CA validation, video and selected `relayProtocol=tls`. Firefox numeric TURN/TLS is rejected with `TLS requires FQDN, not IP address`; this is not a server certificate error and does not prohibit IP HTTPS or direct ICE. A DNS TURN host remains an optional compatibility choice, not a deployment requirement. Do not modify client DNS/hosts automatically.
2. `PUBLIC_BIND_IP`: defaults to the authorized NAS address `192.168.202.11`. Explicitly use `0.0.0.0` only when multiple listening interfaces are intended. DNS may point to another real NAS interface or an authorized TCP forwarder. **Its address need not literally match the bind IP, and this does not prove two addresses belong to one machine.** Workstation `truenas.lan`/`truenas` currently resolves to `192.168.201.11`; it is not the verified `.202.11` NAS.
3. `PUBLIC_PORT=28322`: retains the existing external port. HTTPS and `turns:` must use this same port. Any later WAN exposure requires authorized raw TCP/L4 forwarding, not an HTTPS-only proxy/CDN.
4. `ROOM_NETWORK_CIDR`: the inspected bridge subnet. Keep **`TURN_PRIVATE_IP=auto`** on existing bridges: Docker assigns coturn's private address, HAProxy resolves `turn:3478`, and coturn selects its relay interface. An inspect-reported subnet may be Docker-allocated; it does **not** establish that static endpoint IPs are supported. An explicit unused IP is only for a bridge known to have been created with a user-configured subnet. Never recreate the network carrying existing rooms to enable static addressing.
5. Existing trusted certificate: set the actual NAS `TLS_CERT_FILE` fullchain and `TLS_KEY_FILE`. The certificate must cover both configured hosts, using IP SANs for literals and DNS SANs for names; keep the key mode 0600. The script does not overwrite them.

With an existing certificate:

```sh
python3 deploy/truenas/configure.py --compose-template "$manifest" --settings "$app/native-relay.settings" --public
```

**LAN-only private CA mode:** leave both TLS fields blank and explicitly add `--dev-ca`. IP mode generates an IP SAN and does not require a DNS name. The script creates/reuses `native-relay/tls/`, without importing trust, changing DNS or replacing an existing CA/key. Clients must explicitly trust `native-relay/public/ca.crt`; it is the only public certificate downloadable at same-port `/ca.crt` and contains no private key. Leaf certificates last 30 days. To renew or change configured hosts, explicitly add `--renew-server-cert` to the same command; only the leaf certificate changes, retaining CA, keys and TURN credentials.

Manual client trust, performed by the user only:
1. Download the public certificate from `http://192.168.202.11:28322/ca.crt` after deployment.
2. Import it as a trusted website CA in the intended browser: Firefox **Certificates → Authorities → Import**, or Chromium's certificate manager. Never import the CA key or use a certificate-error bypass.
3. Open `https://192.168.202.11:28322/manage/`. For Node/CDP clients, set `NODE_EXTRA_CA_CERTS` to that downloaded certificate before starting the client. The configurator does not import anything on user machines.

The script generates **`/mnt/ylangsPool/appdata/neko/native-relay/neko.compose.yaml`**. Paste its complete contents into the existing TN **neko** App's YAML editor and save the update. **Do not paste repository source still containing `${...}`**: the TN editor does not read the checkout's `.env`. Generated YAML retains the flat layout and NAS absolute paths, without local cache dependencies. Original `rooms.env/neko.env` retain Compose dotenv semantics; TURN's `relay.env` uses Compose 2.30+ `format: raw`. Generation does not expand permanent passwords or start/stop services.

### Approved one-time topology migration through the same publisher

Before any update, privately save `app.config`, actual installed YAML, original env/auth files, container IDs/start times/init and Chromium PIDs/mounts, and data-root inode/ownership. Copy the default browser's entire **unmounted** `/home/neko/.local/share/pki/nssdb` directory into that backup without inspecting its contents; writable-layer certificate trust otherwise disappears on recreation. Record a restore procedure for an approved stop/copy/start window, never overwrite a live NSS database.

On TN, convert the generated YAML to JSON **without resolving env files** (TN already bundles `yaml`):

```sh
python3 - "$app/native-relay/neko.compose.yaml" "$app/native-relay/neko.compose.json" <<'PY'
import json, os, pathlib, sys, yaml
os.umask(0o077)
pathlib.Path(sys.argv[2]).write_text(json.dumps(yaml.safe_load(pathlib.Path(sys.argv[1]).read_text())))
PY
```

From the source checkout, after obtaining approval for default-service recreation, use the same cached one-command publisher:

```sh
bash deploy/truenas/publish-to-tn.sh \
  --compose /mnt/ylangsPool/appdata/neko/native-relay/neko.compose.json \
  --baseline /mnt/ylangsPool/appdata/neko/backups/YOUR_MIGRATION_SNAPSHOT
```

`--compose` and `--baseline` are NAS paths. The updater preserves the current default `neko/cdp-proxy` definitions and legacy configs exactly, pins the new Manager/room images from `release.json`, pre-pulls all new service images, then makes **one** official `app.update`. It checks external room IDs/start/init and Chromium PIDs/mounts, default image/ports/mounts, auth/config hashes and data-root metadata immediately afterward. Any mismatch stops; there is no silent rollback. `TN_SSH_CONTROL_PATH` may reuse an existing persistent SSH control socket without closing it. Do not run an ordinary old-topology Manager update first.

Rollback, only after checking current impact/approval: load the original backup's `app-config.json` and call `Client().call('app.update', 'neko', {'custom_compose_config_string': json.dumps(saved)}, job=True)`. Restore the original host `compose.yaml` mirror after the API succeeds, and repeat preservation comparisons. Do not replace live profile/downloads/auth data with backup copies merely to roll back configuration. Default-service recreation happens again during rollback.

The root YAML's networkless one-shot `room-image` service runs only `/bin/true`, pulling the **exact** image in Manager's allowlist. It runs no browser and mounts no data; Manager starts after it succeeds. The native stack needs no manual build/load or local retagging. Legacy default `neko/cdp-proxy` retains its existing image, data mounts and `pull_policy: never`; this prevents image replacement, **not** TrueNAS's forced container recreation. Check and approve that process interruption before applying the YAML.

## 2. Migrate old room networking explicitly

Updating Manager/YAML does not change old `http/direct` containers, network labels or ports. An `http` room still uses its old custom aggregator; default Recreate may reject an old image absent from the new allowlist.

Sign in at the new `https://APP_HOST:28322/manage/` with the **original `rooms.env` Owner credentials**. Manager restart invalidates old sessions and temporary Agent keys; sign in/issue keys again. **Do not bulk-delete rooms or modify the default TN-owned neko service.**

Migrate one explicitly selected room at a time:

1. Pause Agent activity and confirm no CDP operation is in flight. Stop the selected room gracefully and back up its actual profile root, including SQLite WAL. A paused container must be unpaused before normal stop; do not copy a profile while it is writing. Record its original running/paused state.
2. Privately back up `GET /manage/api/rooms/ID/settings`: it contains room passwords and must not enter issues/logs. Confirm the actual profile/download mounts (`/profile` to `/home/neko/profile`, `/downloads` to `/home/neko/Downloads`). Do not mount over all of `/etc/neko` or replace Manager's native Neko-3 startup bridge with custom `neko.yaml`.
3. Run this explicit request in the **signed-in Owner browser's same-origin console**, specifying only the chosen NAME. The image defaults to the public channel; if settings pin a version, use the same `ROOM_IMAGE`. It overrides only API/image, retaining name, profile ID, passwords, mounts, resources and policy. `start=false` prevents automatic startup. Settings are validated before removal; retain the private settings backup for recovery if later creation/startup fails.

```js
const name = 'SELECTED-EXISTING-ROOM-NAME';
const image = 'docker.io/ylangylang/neko-chromium-cdp:single-port';
const room = await fetch(`/manage/api/rooms/${encodeURIComponent(name)}/by-name`).then(r => {
  if (!r.ok) throw new Error(`lookup ${r.status}`); return r.json();
});
const result = await fetch(`/manage/api/rooms/${room.id}/recreate?start=false`, {
  method: 'POST', headers: {'Content-Type': 'application/json'},
  body: JSON.stringify({api_version: 3, neko_image: image})
});
if (!result.ok) throw new Error(`recreate ${result.status}`);
console.log('Selected deployment recreated. Start it from the Owner UI and verify it.');
```

4. Start that room from the Owner UI. `/room/NAME/` must show native Neko UI. Sign in with the **original room password** and check media/input/clipboard and retained cookies/downloads. Its Docker `HostConfig.PortBindings` must be empty and network-mode label `relay`. Restore pause state if needed; leave other rooms unchanged.

New rooms also use API **3**, an allowlisted image and managed Profile/Downloads mounts. Neko 3.2's Viper ignores empty environment values: retain Manager's native-YAML startup bridge to disable public IP discovery, rather than merely clearing `NAT1TO1`. Owner Clone accepts one new name, forks an independent durable profile and starts the new deployment.

Issue temporary room-bound Agent Bearer keys from the Owner UI. Discovery/WSS uses same-port `/rooms/NAME/`. Owner cookies and native room passwords are not CDP keys. Attach to the existing browser/context/tabs; pause the Agent before returning to manual control. Neko's control lock does not block CDP. Never send `Browser.close`.

## 3. Channels, version pinning and diagnostics

Defaults are `MANAGER_IMAGE=docker.io/ylangylang/neko-rooms:single-port`, `ROOM_IMAGE=docker.io/ylangylang/neko-chromium-cdp:single-port`, and `RELEASE_PULL_POLICY=always`. The routine command above publishes and updates the authorized TN App through official `app.update`. CI publishes images without logging into the NAS. The explicit publisher/wrapper also publishes images, and only the wrapper's authorized official App update recreates App services.

For pinning/rollback, replace both image settings with the release summary's immutable tags or `repo@sha256:...`, rerun the same configure command, and update the existing App YAML. Existing browser images change only through explicit per-room recreation. For offline archives, explicitly use `RELEASE_PULL_POLICY=never` only after every required image is loaded, referencing the real loaded versions rather than unpublished local tags.

The previously deployed baseline was Manager `single-port-97ffae9` at `sha256:c8c2f4d8aba0228ce33a28d4d0a0d692439a9550bec6c8f17cb3bc1da73db592` and Browser `3.2.0-1` at `sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2`. Both linux/amd64 artifacts were anonymously pulled by digest. New releases emit their own immutable references; unchanged Browser content reuses `single-port-1e57c836bd3d`. The baseline Manager is not a claim about the latest channel after subsequent releases.

Image publishing and YAML generation alone do not establish NAS/WAN reachability. For initial single-port setup, check the configured IP/name, certificate trust and reachability from the actual client. An optional read-only endpoint diagnostic (add `--dev-ca` for private CA mode):

```sh
python3 deploy/truenas/configure.py --settings "$app/native-relay.settings" --check-endpoint
```

It reports the selected DNS address and checks same-port TCP, both names' TLS trust/SAN/expiry, and Owner form 200 without a Basic challenge. It neither forces DNS to equal a bind IP nor proves two interfaces belong to one NAS. On a workstation, `--app-dir` may point to the privately transferred public CA directory. The probe changes no files, performs no login or TURN allocation, and is not a pre-render requirement for a TLS port that is not listening yet.

Focused tooling tests from the source root use temporary directories, without pulling/building/starting containers:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/truenas -p 'test_*.py' -v
# If the manifest checkout is not adjacent: TEST_COMPOSE_TEMPLATE=/path/to/truenas-apps/neko.compose.yaml.
# Optional matching NAS Compose binary: prefix with TEST_COMPOSE_BINARY=/path/to/docker-compose-v2.32.3.
```
