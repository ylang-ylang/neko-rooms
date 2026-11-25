# Storage for neko-rooms

This needs to be specified in docker-compose:

```diff
  neko-rooms:
    image: "m1k1o/neko-rooms:latest"
    restart: "unless-stopped"
    environment:
      - "TZ"
      - "NEKO_ROOMS_EPR"
      - "NEKO_ROOMS_NAT1TO1"
      - "NEKO_ROOMS_TRAEFIK_DOMAIN"
      - "NEKO_ROOMS_TRAEFIK_ENTRYPOINT"
      - "NEKO_ROOMS_TRAEFIK_NETWORK"
      - "NEKO_ROOMS_INSTANCE_URL=http://${NEKO_ROOMS_TRAEFIK_DOMAIN}:8080/" # external URL
+      - "NEKO_ROOMS_STORAGE_ENABLED=true"
+      - "NEKO_ROOMS_STORAGE_INTERNAL=/data"
+      - "NEKO_ROOMS_STORAGE_EXTERNAL=/opt/neko-rooms/data"
    volumes:
      - "/var/run/docker.sock:/var/run/docker.sock"
+      - "/opt/neko-rooms/data:/data"
    labels:
      - "traefik.enable=true"
      - "traefik.http.services.neko-rooms-frontend.loadbalancer.server.port=8080"
      - "traefik.http.routers.neko-rooms.entrypoints=${NEKO_ROOMS_TRAEFIK_ENTRYPOINT}"
      - "traefik.http.routers.neko-rooms.rule=Host(`${NEKO_ROOMS_TRAEFIK_DOMAIN}`)"
```

Where:
- `NEKO_ROOMS_STORAGE_INTERNAL` is the directory inside the container.
- `NEKO_ROOMS_STORAGE_EXTERNAL` is the directory outside container.
- `"/opt/neko-rooms/data:/data"` is volume mount.

Please note, that **neko-rooms** must be aware of external storage path, as it is going to mount it to the room itself. That needs to be available to **neko-rooms** as well, in order to manage that folder.

Inside storage path (e.g. `/opt/neko-rooms/data`) there will be available these mount points:

- `/opt/neko-rooms/data/`**`profiles/<profile ID>/`** stores new durable browser profiles and managed Downloads.
- `/opt/neko-rooms/data/`**`profiles.json`** is the atomic private ownership registry.
- `/opt/neko-rooms/data/`**`rooms/<old room name>/`** remains in place for adopted legacy profiles; it is no longer owned by the deployment name.
- `/opt/neko-rooms/data/`**`templates/`** where templates will be accessible.

## Durable browser profiles

Use **Profiles** in the Manager UI to create/list profiles and explicitly delete an available profile. **Add room** can select an available profile or create a new one. Omitting `profile_id` creates a durable profile automatically, including Quick room. A profile has its own stable ID and display name; image, screen, networking and room passwords belong to the deployment, not the profile.

Each profile has one exclusive deployment binding, including while the deployment is stopped. Remove that deployment to release the profile. Creating another deployment with a bound profile returns **409**. Removing a deployment never deletes its profile or Downloads. To transfer browser identity, remove the old deployment, then select its retained profile when creating a replacement. Explicit profile deletion is irreversible and rejected while bound.

**Clone** still needs only one new deployment name. It forks a new durable profile with independent files (copy-on-write reflinks where available, never hardlinks/shared writable mounts), then binds and starts the new deployment. Later source/clone writes remain independent.

Manager registers legacy `rooms/NAME/` directories in place, without moving/copying live data or restarting browsers. Legacy orphan directories are discoverable too. Deployment names may be reused without colliding with retained profiles. Docker labels and actual managed mounts reconcile durable bindings after Manager restart or external container removal; stale leases do not depend on RAM. One Manager owns each configured storage root.

The complete opaque Chromium user-data directory remains on disk: Local State, SQLite cookies and journals, extension files/settings, history, localStorage and IndexedDB. For supported CDP-enabled Chromium images, normal Stop/Restart/Remove first captures freshly buffered cookies (including session/HttpOnly cookies) and active localStorage, then gracefully stops the container. Capture errors abort removal instead of silently discarding identity. A private **0600** `.neko-identity.json` checkpoint lives under the profile root, outside the browser bind mounts; it is atomically written and consumed on the next actual browser start. Startup restoration uses only owned inert targets and does not contact websites. Existing source tabs/contexts are never navigated or closed.

The checkpoint contains private authentication data. Back up the **whole profile root and registry** privately, not just the mounted `profile/` subdirectory. Never put profiles, checkpoints or credentials in Git, API responses, logs or shared task notes. Registry/list API responses expose only IDs, names, creation times and deployment binding. Other browser images retain their opaque files through graceful stop; live supplementation requires the private Chromium CDP endpoint. External forced container removal cannot capture data that only existed in browser RAM; use Manager removal for identity-preserving replacement.

Owner-only API paths (relative to `admin.path_prefix`):

- `GET /api/profiles`: public ownership metadata only, including available retained profiles.
- `POST /api/profiles` with `{"name":"Work account"}`: create an available profile.
- `DELETE /api/profiles/ID`: explicitly delete an available profile and all its data.
- `POST /api/rooms` with `profile_id`: bind an available profile. Omit it to create a new durable profile, optionally named by `profile_name`.

These are Manager Owner operations, not native viewer or Agent Bearer operations. Existing passwords, CA/keys and Agent authorization remain separate.

## How can it be used?

You can mount e.g. browser policies and this way customize browser.

You can always refer to [google-chrome Dockerfile](https://github.com/m1k1o/neko/blob/1800d077d8138bdb23c25028bf4201a0469f91aa/.m1k1o/google-chrome/Dockerfile). In this case, policies are mounted to `/etc/opt/chrome/policies/managed/policies.json` path inside container. So you can mount custom file to this location what overwrites its content.

For this purpose, template path type is recommended, as policy file should only be readonly and can be reused along multiple rooms. You can then store your policies file to e.g. `/opt/neko-rooms/data/templates/policies.json` and have it mounted to all rooms.

![Storage](storage.png)

## Mount path whitelist

If you want to mount any path from your filesystem, you need to whitelist it first.

Add it as environment variables to your docker compose:

```yaml
NEKO_ROOMS_MOUNTS_WHITELIST: "/media"
```

Or when using multiple, they can be separated white space:

```yaml
NEKO_ROOMS_MOUNTS_WHITELIST: "/home /media"
```

You can mount any path within your whitelisted path. Meaning, if you whitelisted `/home` folder you can selectively mount path e.g. `/home/ubuntu` to a room.

**NOTICE:** You could whitelist all paths on your system with `/`. From security perspective, this solution is *strongly discouraged*.
