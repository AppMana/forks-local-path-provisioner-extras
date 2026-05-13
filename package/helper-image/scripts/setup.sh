#!/bin/sh
# setup.sh — create the volume directory and apply per-filesystem quota.
#
# env: VOL_DIR VOL_SIZE_BYTES VOL_MODE VOL_QUOTA_TYPE
set -eu
. /usr/local/sbin/common.sh

[ -n "${VOL_DIR:-}" ] || die "VOL_DIR not set"
parent=$(dirname "$VOL_DIR")
qtype=$(resolve_quota_type "$parent")

if [ "$qtype" = "btrfs" ]; then
    # btrfs: the volume directory must be a subvolume so it gets its own qgroup.
    if [ -d "$VOL_DIR" ]; then
        btrfs subvolume show "$VOL_DIR" >/dev/null 2>&1 \
            || die "$VOL_DIR exists but is not a btrfs subvolume; remove it and retry"
    else
        btrfs subvolume create "$VOL_DIR"
        chmod 0777 "$VOL_DIR"
    fi
else
    mkdir -m 0777 -p "$VOL_DIR"
fi

if [ "${VOL_MODE:-Filesystem}" = "Block" ]; then
    log "Block mode volume — quota enforcement not applicable, directory created"
    exit 0
fi

apply_quota "$VOL_DIR" "${VOL_SIZE_BYTES:-0}" "$qtype"
log "setup complete: $VOL_DIR"
