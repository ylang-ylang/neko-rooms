# Room cloning

An authenticated Owner can clone a room with one new name:

```http
POST /manage/api/rooms/{roomId}/clone
Content-Type: application/json

{"name":"new-room"}
```

The management prefix is configurable. The request accepts only `name`, with no
settings, credentials or `start` query parameter. Success is **201** with the same
`RoomEntry` shape as room creation, after the independent target has started.
The management API's existing Owner authentication and same-origin checks apply;
a temporary Agent key cannot clone rooms.

## What is preserved

The backend reads the canonical source settings itself: image, screen, codecs,
custom environment, passwords, labels, resource limits, DNS, explicit hostname,
browser policy and mount destinations. The new name, default name-bound hostname,
container identity and allocated network resources are regenerated. Relay/HTTP
rooms retain the current global private-network policy and publish no additional
host ports. Direct rooms receive newly allocated ports.

The **entire managed per-room storage root** is copied, not just a cookie database
or selected profile files. This retains browser profiles, Local State, SQLite
WALs/journals, localStorage, IndexedDB, extensions and managed downloads. Private
mounts resolve into the new room's own storage. Read-only templates can be reused.
Unmanaged writable mounts, non-bind volumes and symbolic mount-root paths are
rejected explicitly rather than shared with the source.

The snapshot preserves ownership, modes and symlinks without traversing symlinks
outside the storage tree. Linux reflinks are used where supported, with a real
copy fallback; writable files are never hardlinked. Chrome singleton locks,
DevToolsActivePort and Unix sockets are process artifacts and are omitted.
A private staging directory is committed with atomic **no-replace** rename;
existing rooms or storage directories are never overwritten.

## Live browser identity

A frozen filesystem is crash-consistent, but Chromium can still have new cookies
and localStorage writes buffered in memory. Live cloning therefore also captures:

- Default-context cookies through `Storage.getCookies`, including HttpOnly and
  session cookies. The target default cookie jar is replaced through
  `Storage.clearCookies` and `Storage.setCookies`; session cookies omit expiry.
- Active HTTP(S) frame localStorage through `DOMStorage.getDOMStorageItems` and
  each frame's actual storage key. Site-isolated iframe targets are joined to
  page frame trees by parent frame ID, preserving their partition topology.

These supplemental values stay in backend memory, never API responses or logs.
The target restores localStorage in disposable, inert same-origin documents
served through CDP Fetch interception, without contacting the websites. Those
**target-only** pages are closed afterward. Source pages and contexts are never
created, navigated or closed. Full-profile copying remains necessary for opaque
browser identity and IndexedDB; cookie export is not a replacement for it.

A running source is paused only for the filesystem copy and resumed before target
startup. An already-paused source is briefly resumed for read-only CDP capture,
then returned to paused state before its filesystem snapshot. A stopped source
uses its persisted files without being started. Lifecycle mutations and clone
transactions share a Manager lock. Cancellation and failures restore source
state and remove only the transaction's own target resources; a Docker cleanup
failure is surfaced explicitly. Live cloning requires the private Chromium CDP
endpoint; unavailable identity capture fails instead of silently producing an
unauthenticated copy. JavaScript heap and sessionStorage are not captured.

## Errors

| Status | Meaning |
| --- | --- |
| 400 | Invalid name/request or unsupported source settings/mounts |
| 401 | Owner authentication required |
| 403 | Cross-origin mutation denied |
| 404 | Source room does not exist |
| 409 | Target room name or persistent storage already exists |
| 500 | Snapshot, browser, Docker, state-restoration or cleanup failure |

## Validation

```sh
go test ./...
go test -race ./internal/room ./internal/api ./internal/server ./internal/types
NEKO_CLONE_TEST_CHROMIUM="$(command -v chromium)" \
  go test -count=1 -v ./internal/room -run '^TestCloneChromiumFreshIdentity$'
```

The opt-in Chromium test uses only new disposable local profiles and private test
origins. It checks fresh HttpOnly persistent/session cookies through actual HTTP
account authentication, localStorage, partitioned iframe localStorage, IndexedDB,
source/target write isolation and absence of website requests during restoration.
It does not access Docker or existing rooms. Deployment acceptance additionally
exercises the Owner API against an explicitly owned canary room and its actual
image, persistent mounts and browser startup command.
