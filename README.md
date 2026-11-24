# neko-rooms

<p align="center">
  <img src="https://img.shields.io/github/v/release/m1k1o/neko-rooms" alt="release">
  <img src="https://img.shields.io/github/license/m1k1o/neko-rooms" alt="license">
  <img src="https://img.shields.io/docker/pulls/m1k1o/neko-rooms" alt="pulls">
  <img src="https://img.shields.io/github/issues/m1k1o/neko-rooms" alt="issues">
  <a href="https://discord.gg/3U6hWpC" ><img src="https://discordapp.com/api/guilds/665851821906067466/widget.png" alt="Chat on discord"></a>
</p>

Simple room management system for [n.eko](https://github.com/m1k1o/neko). Self hosted rabb.it alternative.

<div align="center">
  <img src="https://github.com/m1k1o/neko-rooms/raw/master/docs/rooms.png" alt="rooms">
  <img src="https://github.com/m1k1o/neko-rooms/raw/master/docs/new_room.png" alt="new room">
  <img src="https://github.com/m1k1o/neko-rooms/raw/master/docs/neko.gif" alt="n.eko">
</div>

## Agent-access fork

This fork adds temporary, per-room Chromium DevTools Protocol (CDP) access to the **same browser and tabs shown in Neko**, plus an **Agent** action in the room list. It does not launch a separate browser. The installation examples below use upstream images; build this fork's `Dockerfile` to include these features and the updated management UI.

### Setup

- Use a Chromium room image with CDP enabled on loopback port **9222** and a room-side relay listening on **9223**, reachable from the manager on the room's Docker network. The manager connects to the current room container's `:9223`; it does **not** install that relay or enable CDP for you. Do not expose the unauthenticated relay publicly.
- Protect management with the existing admin username/password (form login by default) or proxy authentication. Set `NEKO_ROOMS_ADMIN_PATH_PREFIX=/manage`; the management UI is then at `/manage/`. Agent keys never grant management access.
- Set the public CDP prefix, including the entire path before the room name:

  ```yaml
  NEKO_ROOMS_AGENT_PUBLIC_URL: https://cdp.example.com/rooms/
  ```

  This is a **full prefix**, not just an origin. For an external `/cdp/rooms/` prefix, use `https://cdp.example.com/cdp/rooms/` and have the reverse proxy map it to the manager's `/rooms/` routes. Without this variable, URLs are inferred from the request and forwarded host/protocol headers, with `/rooms/` as the path. Configure trusted proxy headers correctly or set the explicit prefix.
- Route public CDP HTTP and WebSocket traffic directly to the manager, preserving `Authorization: Bearer …` and WebSocket upgrade headers. Do not put a Basic-auth/login-page challenge in front of `/rooms/`; management remains separately protected.

### Management login

Open `/manage/` and sign in with the **same** `NEKO_ROOMS_ADMIN_USERNAME` / `NEKO_ROOMS_ADMIN_PASSWORD` already configured (`admin` is the default username). Local credentials default to `NEKO_ROOMS_ADMIN_AUTH_MODE=form`: a normal HTML form submits by POST, with named username/password fields and `autocomplete="username"` / `autocomplete="current-password"` for password managers. Form mode never accepts or challenges with HTTP Basic. Set `NEKO_ROOMS_ADMIN_AUTH_MODE=basic` explicitly for the legacy browser prompt; `NEKO_ROOMS_ADMIN_PROXY_AUTH` still takes precedence. Without credentials or proxy auth, management remains unauthenticated.

**Startup compatibility:** A partial username/password pair now fails startup instead of silently opening management. Configure both (the default username is `admin`) or intentionally neither.

The host-only, HttpOnly, SameSite=Lax owner cookie lasts **7 days**; sessions live only in manager RAM. **Restarting the manager requires signing in again.** Use **退出登录** in the header to revoke the session; expired/revoked sessions return to the login page. HTTPS (or HTTPS forwarded by an explicitly trusted `NEKO_ROOMS_PROXY=true` proxy) adds the Secure cookie flag. `GET /manage/auth/status` returns `{mode, authenticated, username?, login_url, logout_url}` without exposing credentials or session tokens.

Programmatic owner API clients must first POST the form and retain its cookie, not use an Agent Bearer key. Login, logout and all mutating owner API requests require an `Origin` (or `Referer`) matching the manager's scheme, host **and port**. For example, with `OWNER_USERNAME` and `OWNER_PASSWORD` set to the existing credentials:

```sh
ORIGIN=https://rooms.example.com
MANAGER="$ORIGIN/manage"
COOKIE_JAR=$(mktemp)
trap 'rm -f "$COOKIE_JAR"' EXIT
curl --fail --silent --show-error -c "$COOKIE_JAR" -o /dev/null \
  -H "Origin: $ORIGIN" \
  --data-urlencode "username=$OWNER_USERNAME" --data-urlencode "password=$OWNER_PASSWORD" \
  --data-urlencode 'next=/manage/' "$MANAGER/auth/login"
curl --fail --silent --show-error -b "$COOKIE_JAR" "$MANAGER/api/rooms"
curl --fail --silent --show-error -b "$COOKIE_JAR" -c "$COOKIE_JAR" -o /dev/null \
  -H "Origin: $ORIGIN" -X POST "$MANAGER/auth/logout"
```

Owner sessions, legacy CDP Basic credentials and Neko room-viewer passwords are separate credentials; none substitutes for the temporary Agent CDP Bearer key described below.

### Give an agent access

Open a room's **Agent** dialog, choose a lifetime (15 minutes, 1 hour, or 8 hours), optionally add a label, and create a key. Copy the returned handoff prompt verbatim; it contains the URL, temporary Bearer key, expiry and instructions to reuse existing tabs. If automatic clipboard access fails, use the visible, selectable prompt or the copy button.

The raw key and prompt are returned **only when created**. Closing the dialog does not revoke the key; reopening shows metadata and connection counts, not the secret. Refresh the list to update counts, and use **Revoke** to end access. The API defaults to 1 hour and accepts `ttl_seconds` from 1 to 86400 (24 hours). Keys are held in memory by hash: expiry or revocation closes existing sockets as well as rejecting new connections, and **restarting the manager invalidates all keys**.

Keys bind to the stable room **name**, not its container ID. Each HTTP discovery request and WebSocket connection resolves that name to the current managed container, so room recreation preserves the key's identity until expiry. Renaming or reusing a name changes which room that name identifies. A stopped, paused or missing room cannot be used for CDP access.

| Route (with `/manage` configured) | Authentication / purpose |
| --- | --- |
| `POST /manage/api/agent/rooms/{name}/keys` | Native management auth; JSON `{ "ttl_seconds": 3600, "label": "assistant" }` returns key and canonical prompt |
| `GET /manage/api/agent/rooms/{name}/keys` | Native management auth; active key metadata and connection counts |
| `DELETE /manage/api/agent/rooms/{name}/keys/{key_id}` | Native management auth; revoke and close that key's sessions |
| `GET /rooms/{name}/json/version`, `GET /rooms/{name}/json/list` | Temporary Bearer header; CDP discovery with public debugger URLs |
| `GET /rooms/{name}/devtools/browser/*`, `GET /rooms/{name}/devtools/page/*` | Temporary Bearer header on the WebSocket upgrade |

Use the same `Authorization: Bearer …` header for **both discovery and WebSocket connections**; never put the key in a URL/query or use page-global headers as CDP authentication. For example, with Playwright in Node.js:

```js
const browser = await chromium.connectOverCDP('https://cdp.example.com/rooms/example-room/', {
  headers: { Authorization: `Bearer ${process.env.AGENT_KEY}` },
});
console.log(browser.contexts()[0].pages().map(page => page.url()));
```

This is shared-browser control, not a sandbox: use existing contexts/pages, do not launch another browser or close shared tabs/the browser, and revoke the key when finished. Neither Neko login passwords nor long-lived management credentials belong in the agent handoff.

### Room presets

New-room defaults use Neko API v3, 1920×1080 at 30 fps, 2 GiB shared memory and developer tools enabled. Separate private **Profile** (`/home/neko/profile`) and **Downloads** (`/home/neko/Downloads`) mounts preserve their data when configured with [storage](./docs/storage.md). Toggling persistent browser data affects only the Profile mount, not Downloads. Existing rooms are not migrated automatically; use compatible images and storage settings.

## Fixed-public-port relay deployment

For the relay-capable fork, use the [private-network relay example and operator workflow](./examples/relay/). It terminates HTTPS/UI/management/Agent WebSockets on TCP **443** and TURN/TLS media on TCP **5349** (both configurable), with ordinary TURN relay allocations confined to the private room Docker network—not a public UDP range moved to another server. Existing owner form login, temporary Agent Bearer keys, room image and Profile/Downloads storage contracts remain unchanged.

The example includes strict configuration checks and a loopback-only gateway/TURN fixture test. Public activation still requires the user's actual gateway machine, domain and matching certificate, plus a preloaded relay-capable manager release; it does not deploy to the NAS or automatically migrate existing dynamic rooms. TrueNAS activation/rollback must use official `app.update` with Custom App YAML: all App services, including the default browser, are recreated, while manager-created dynamic rooms remain running. See the example for the platform-specific interruption/data-preservation instructions, canary checks, rollback and split-host private routing.

## Zero-knowledge installation (with HTTPS)

No experience with Docker and reverse proxy? No problem! Follow these steps to set up your Neko Rooms quickly and securely:

- Rent a VPS with public IP and OS Ubuntu.
- Get a domain name pointing to your IP (you can even get some for free).
- Run install script and follow instructions.
- Secure using HTTPs thanks to Let's Encrypt and Traefik or NGINX.

```bash
wget -O neko-rooms-traefik.sh https://raw.githubusercontent.com/m1k1o/neko-rooms/master/traefik/install
sudo bash neko-rooms-traefik.sh
```

### Community Installation Scripts

We have community-contributed installation scripts available. Check out our [community installation guides](./community/README.md) for instructions on installing neko-rooms on various Linux distributions. These scripts are maintained by the community and support different Linux distributions like Arch Linux, Fedora, and more.

## How to start

If you want to use Traefik as reverse proxy, visit [installation guide for traefik as reverse proxy](./traefik/).

Otherwise modify variables in `docker-compose.yml` and just run `docker-compose up -d`.

### Download images / update

You need to pull all your images, that you want to use with neko-room. Otherwise, you might get this error: `Error response from daemon: No such image:` (see issue #1).

```sh
docker pull ghcr.io/m1k1o/neko/firefox
docker pull ghcr.io/m1k1o/neko/chromium
# etc...
```

If you want to update neko image, you need to pull new image and recreate all rooms, that use old image. To update neko rooms on a **standalone Docker Compose installation**, run the commands below. For a TrueNAS-managed App, use the official `app.update`/Custom App YAML workflow described in the [operator instructions](./examples/relay/#3-controlled-activation-and-negative-checks), not manual Compose updates.

```sh
docker-compose pull
docker-compose up -d
```

### Enable storage

You might have encountered this error:

> Mounts cannot be specified because storage is disabled or unavailable.

If you didn't specify storage yet, you can do it using [this tutorial](./docs/storage.md).

### Use nvidia GPU

If you want to use nvidia GPU, you need to install [nvidia-docker](https://github.com/NVIDIA/nvidia-docker).

Change neko images to nvidia images in `docker-compose.yml` using envorinment variable `NEKO_ROOMS_NEKO_IMAGES`:

```bash
NEKO_ROOMS_NEKO_IMAGES="
  ghcr.io/m1k1o/neko/nvidia-chromium:latest
  ghcr.io/m1k1o/neko/nvidia-google-chrome:latest
  ghcr.io/m1k1o/neko/nvidia-microsoft-edge:latest
  ghcr.io/m1k1o/neko/nvidia-brave:latest
"
```

When creating new room, you need to specify to use GPU in expext settings.

### Docs

For more information visit [docs](./docs).

### Roadmap:
 - [x] add GUI
 - [x] add HTTPS support
 - [x] add authentication provider for traefik
 - [x] allow specifying custom ENV variables
 - [x] allow mounting directories for persistent data
 - [x] optionally remove Traefik as dependency
 - [ ] add upgrade button
 - [ ] auto pull images, that do not exist
 - [ ] add bearer token to for API
 - [ ] add docker SSH / TCP support
 - [ ] add docker swarm support
 - [ ] add k8s support
