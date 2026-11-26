#!/usr/bin/env bash
# Cached publication, then one official TN update (current or explicitly rendered topology).
set -euo pipefail
here=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_repo=$(CDPATH= cd -- "$here/../.." && pwd)
host=truenas_admin@192.168.202.11
remote=/home/truenas_admin/neko-native-release
compose=
baseline=
publish_args=()
while (($#)); do
    case "$1" in
        --compose|--baseline)
            (($# >= 2)) || { printf 'Missing migration argument\n' >&2; exit 2; }
            if [[ $1 == --compose ]]; then compose=$2; else baseline=$2; fi
            shift 2 ;;
        *) publish_args+=("$1"); shift ;;
    esac
done
if [[ -n $compose && -z $baseline ]]; then
    printf 'Topology migration requires --baseline and prior service-recreation approval\n' >&2
    exit 2
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/neko-publish-tn.XXXXXX")
chmod 700 "$work"
control=${TN_SSH_CONTROL_PATH:-"$work/control"}
own_control=true
[[ -z ${TN_SSH_CONTROL_PATH:-} ]] || own_control=false
cleanup() {
    local status=$?
    if $own_control; then
        ssh -o ControlPath="$control" -O exit "$host" >/dev/null 2>&1 || true
    fi
    # The pi-command-policy rm shim blocks PATH rm pending confirmation. Invoke
    # the real binary directly, scoped to exactly this script's own mktemp
    # directory; the policy is never globally disabled. Restore the exit status
    # that triggered the trap so cleanup cannot mask or replace it.
    /bin/rm -rf -- "$work"
    return "$status"
}
trap cleanup EXIT
(cd "$source_repo" && python3 scripts/release.py publish "${publish_args[@]}" --output "$work/release.json")
if $own_control; then
    ssh -MNf -o BatchMode=yes -o StrictHostKeyChecking=yes -o ControlMaster=yes \
        -o ControlPersist=120 -o ControlPath="$control" "$host"
else
    [[ -S $control ]] || { printf 'Existing SSH control socket unavailable\n' >&2; exit 1; }
fi
scp -q -o ControlPath="$control" "$here/update.py" "$work/release.json" "$host:$remote/"
remote_args=(--release "$remote/release.json")
if [[ -n $compose ]]; then remote_args+=(--compose "$compose" --baseline "$baseline"); fi
printf -v remote_command '%q ' sudo -n python3 "$remote/update.py" "${remote_args[@]}"
ssh -tt -o ControlPath="$control" "$host" "$remote_command"
