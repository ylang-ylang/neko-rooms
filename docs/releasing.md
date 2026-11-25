# Single-port releases

**Normal operation:** push committed code to `release/single-port`, `main` or
`master`, or dispatch **Single-port checks and release** on a branch. Go/frontend
unit checks and image builds run in parallel; only a successful pair promotes the
channels. PRs run checks only. No agent review, full-image pull verification,
SSH, NAS restart or production/media/clone acceptance matrix runs after release.

## One-time bootstrap

- Enable Actions in this fork and allow the actions in `.github/workflows/build.yml`.
- Set repository secrets **`DOCKERHUB_USERNAME`** (`ylangylang`) and
  **`DOCKERHUB_TOKEN`** (Docker Hub write token). Use GitHub's secret UI or the
  private prompt/stdin of `gh secret set DOCKERHUB_TOKEN --repo ylang-ylang/neko-rooms`.
  Never put tokens in Git, arguments or logs. `GITHUB_TOKEN` has `contents: read`;
  it cannot authenticate to Docker Hub. No cross-repository PAT/bot is required.
- Confirm both Docker Hub repositories below are writable and have the intended
  visibility. Public source does not imply public images; confirm anonymous pull
  once when making the images public, not after every release.
- GitHub **`ylang-ylang/neko-rooms`** explicitly maps to Docker Hub **`ylangylang`**.
  Another fork must set repository variable `DOCKERHUB_NAMESPACE` to its writable
  namespace; the GitHub owner is never used as a guessed Docker Hub account.
- Authenticate Git if needed, then `git push origin HEAD:release/single-port`.
  Subsequent pushes publish automatically. The dispatch UI additionally requires
  the workflow on the repository's default branch. Missing Git authentication,
  disabled fork Actions or absent secrets are bootstrap blockers: a local workflow
  file does **not** mean the remote pipeline has run.

## Tags and fast rebuilds

| Component | Following channel | Immutable artifact |
| --- | --- | --- |
| Manager | `docker.io/ylangylang/neko-rooms:single-port` | `:single-port-<Git SHA7>` |
| Chromium CDP | `docker.io/ylangylang/neko-chromium-cdp:single-port` | `:single-port-<adapter hash12>` |

`python3 scripts/release.py plan` owns all refs, hashes and cache scopes. The
browser's v1 SHA256 encoding includes `linux/amd64`, then sorted relative path,
Git mode, byte length and bytes of its Dockerfile (including pinned base), literal
local COPY sources, and adapter `.dockerignore` if present. README/tests not copied
into the image do not change the key; neither do timestamps or plan JSON spacing.
New literal COPY inputs are included automatically; unsupported COPY/ADD syntax
fails instead of reusing a stale image.

`plan --probe` queries remote **manifests only**. An existing immutable tag is
reused, never intentionally overwritten (including prebuilt `single-port-97ffae9`).
Manager-only changes therefore do not pull/build Chromium. First/missing keyed
artifacts build automatically; auth/network errors fail explicitly, not as cache
misses. An accepted browser artifact can receive its computed content alias with
manifest retagging alone; never assign an existing key to different content.

Only **linux/amd64** is built. Manager dependency layers use `npm ci` after the
lock/config COPY and `go mod download` after `go.mod`/`go.sum`; unrelated frontend,
docs or release files cannot invalidate the Go source layer. Buildx exports
`mode=max` to separate per-image registry cache tags and GHA scopes. Local builds
reuse the same registry cache. Cache mounts help persistent builders but are not
assumed to survive GHA export. The release UI compiles once inside Docker, not
again in a separate CI build; PRs compile it without publishing.

## One-command manual fallback

With Python 3.9+, Git/tar, Docker/buildx and an existing authorized Docker Hub login
(no host Go, Node or npm is required; images compile Go and the UI inside Docker):

```sh
python3 scripts/release.py publish --dry-run  # offline plan, no side effects
python3 scripts/release.py publish           # probe → build/reuse missing → push; release.json
```

This automatically archives the selected commit and builds missing images in parallel
with a persistent `neko-release` docker-container builder (created if absent, without
changing the default). It runs no Go/frontend/adapter component checks: CI already
gates channel promotion on them, and the separate `check` subcommand still runs each
component on demand.

Dirty/untracked files cannot enter an image. `--source <commit>` and `--namespace`
select explicit alternatives. No credentials are copied and no Pi tools are used.
Do not run manual publication simultaneously with Actions on another host.
`promote` is the low-level CI stage; use `publish` for the complete command.

## TrueNAS handoff

Release branches share one concurrency group; a stale branch-tip rerun cannot
promote over a newer commit. After normal tests/builds succeed, channels are copied
by digest without transferring browser layers through the runner. The
**single-port-release** Actions artifact contains `release.json`, with each
component's `immutable`, `channel`, `digest` and `pinned` (`image@sha256`) refs. The
job summary also lists them; secret-masked usernames in summaries do not affect
the artifact. Download via Actions, or `gh run download <run-id> --repo
ylang-ylang/neko-rooms --name single-port-release`.

TrueNAS may follow `single-port` and pull at an operator-chosen upgrade time, or
use both `pinned` refs as a locked pair. Publication does not restart NAS/rooms or
migrate Profile/Downloads. Two registry tags cannot update atomically: if a channel
write fails, the command fails with `channels_promoted: false` in the manifest.
Rerun that release or use its pinned pair before upgrading. Successful writes set
`channels_promoted: true`; no second acceptance procedure follows. `manifest
--source <commit> --output release.json` resolves an existing release without
building or promoting it (prefix with `python3 scripts/release.py`).

Implementation checks: `python3 -B -m unittest discover -s scripts -p 'test_*.py'`,
`python3 scripts/release.py check --component adapter`, `actionlint`, and the offline
plan/dry-run. They cover source-change skips, cache boundaries, immutable reruns,
auth failures and failed-build promotion gates; no real-room matrix is required.

Official references: [GitHub publishing](https://docs.github.com/en/actions/use-cases-and-examples/publishing-packages/publishing-docker-images),
[concurrency](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency),
[Docker cache](https://docs.docker.com/build/ci/github-actions/cache/),
[registry cache](https://docs.docker.com/build/cache/backends/registry/),
[manifest inspect](https://docs.docker.com/reference/cli/docker/buildx/imagetools/inspect/),
[manifest copy](https://docs.docker.com/reference/cli/docker/buildx/imagetools/create/).
