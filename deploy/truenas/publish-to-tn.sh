#!/usr/bin/env bash
# Build/publish via the existing incremental publisher, then update current TN topology.
set -euo pipefail
here=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_repo=$(CDPATH= cd -- "$here/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/neko-publish-tn.XXXXXX")
chmod 700 "$work"
host=truenas_admin@192.168.202.11
remote=/home/truenas_admin/neko-native-release
control="$work/control"
cleanup() {
    ssh -o ControlPath="$control" -O exit "$host" >/dev/null 2>&1 || true
    rm -rf -- "$work"
}
trap cleanup EXIT
(cd "$source_repo" && python3 scripts/release.py publish "$@" --output "$work/release.json")
ssh -MNf -o BatchMode=yes -o StrictHostKeyChecking=yes -o ControlMaster=yes \
    -o ControlPersist=120 -o ControlPath="$control" "$host"
scp -q -o ControlPath="$control" "$here/update.py" "$work/release.json" "$host:$remote/"
ssh -tt -o ControlPath="$control" "$host" \
    "sudo -n python3 $remote/update.py --release $remote/release.json"
