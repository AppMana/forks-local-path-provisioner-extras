#!/bin/sh
# restore.sh — create a new read-write volume directory from a snapshot, then
# apply the requested quota.
#
# env: SNAP_DIR (source snapshot), VOL_DIR (new volume dir), VOL_SIZE_BYTES,
#      VOL_QUOTA_TYPE
#
# Per-filesystem clone primitive (mirrors snapshot.sh):
#   btrfs : btrfs subvolume snapshot SRC DST   (read-write clone)
#   xfs   : cp --reflink=always -aR SRC DST
set -eu
. /usr/local/sbin/common.sh

[ -n "${SNAP_DIR:-}" ] || die "SNAP_DIR not set"
[ -n "${VOL_DIR:-}" ]  || die "VOL_DIR not set"
[ -e "$SNAP_DIR" ]     || die "snapshot $SNAP_DIR does not exist"
parent=$(dirname "$VOL_DIR")
mkdir -p "$parent"
fs=$(resolve_fs "$parent")

if [ -e "$VOL_DIR" ]; then
    log "destination $VOL_DIR already exists, treating restore as idempotent"
else
    case "$fs" in
    btrfs)
        command -v btrfs >/dev/null 2>&1 || die "btrfs not found (install btrfs-progs)"
        log "btrfs: read-write subvolume snapshot $SNAP_DIR -> $VOL_DIR"
        btrfs subvolume snapshot "$SNAP_DIR" "$VOL_DIR"
        ;;
    xfs)
        log "xfs: reflink clone $SNAP_DIR -> $VOL_DIR"
        cp --reflink=always -aR "$SNAP_DIR" "$VOL_DIR"
        chmod -R u+w "$VOL_DIR" 2>/dev/null || true
        ;;
    *)
        # Fall back to a plain recursive copy (no reflink savings, but works).
        log "filesystem $fs has no clone primitive, falling back to cp -a"
        cp -aR "$SNAP_DIR" "$VOL_DIR"
        ;;
    esac
    chmod 0777 "$VOL_DIR"
fi

# A restored xfs/ext4 volume needs its own project ID, so resolve the quota
# type against the destination filesystem rather than trusting the source's.
qtype=$(resolve_quota_type "$parent")
apply_quota "$VOL_DIR" "${VOL_SIZE_BYTES:-0}" "$qtype"
log "restore complete: $VOL_DIR (from $SNAP_DIR, fs=$fs)"
