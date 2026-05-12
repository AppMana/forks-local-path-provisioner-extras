#!/bin/sh
# snapshot.sh — create or delete a read-only snapshot of a volume directory.
#
# Two modes (selected by the "-a <action>" argument the CSI controller appends):
#   -a snapshot          create a read-only clone at SNAP_DIR + a JSON sidecar
#   -a delete-snapshot   remove SNAP_DIR (and SNAP_DIR.json)
#
# env: VOL_DIR (source volume), SNAP_DIR (snapshot dir), VOL_SIZE_BYTES,
#      VOL_QUOTA_TYPE
#
# Per-filesystem clone primitive:
#   btrfs : btrfs subvolume snapshot -r SRC DST   (atomic, copy-on-write)
#   xfs   : cp --reflink=always -aR SRC DST       (requires reflink; NOT atomic
#           across in-flight writes — quiesce the workload first)
#   ext4 / other : unsupported — snapshots require reflink or subvolumes
set -eu
. /opt/local-path-provisioner/common.sh

# Parse "-a <action>" (default: snapshot).
action=snapshot
prev=
for arg in "$@"; do
    case "$prev" in -a) action=$arg ;; esac
    prev=$arg
done

[ -n "${SNAP_DIR:-}" ] || die "SNAP_DIR not set"
snap_parent=$(dirname "$SNAP_DIR")
sidecar="${SNAP_DIR}.json"

if [ "$action" = "delete-snapshot" ]; then
    if command -v btrfs >/dev/null 2>&1 && btrfs subvolume show "$SNAP_DIR" >/dev/null 2>&1; then
        btrfs subvolume delete "$SNAP_DIR"
    else
        rm -rf "$SNAP_DIR"
    fi
    rm -f "$sidecar"
    log "delete-snapshot complete: $SNAP_DIR"
    exit 0
fi

# --- create ---
[ -n "${VOL_DIR:-}" ] || die "VOL_DIR not set"
[ -d "$VOL_DIR" ]     || die "source volume $VOL_DIR does not exist"
mkdir -p "$snap_parent"
fs=$(resolve_fs "$(dirname "$VOL_DIR")")

if [ -e "$SNAP_DIR" ]; then
    # Idempotent: a previous attempt already created it.
    log "snapshot $SNAP_DIR already exists, treating as idempotent"
else
    case "$fs" in
    btrfs)
        command -v btrfs >/dev/null 2>&1 || die "btrfs not found (install btrfs-progs)"
        btrfs subvolume show "$VOL_DIR" >/dev/null 2>&1 \
            || die "source $VOL_DIR is not a btrfs subvolume; was it created by this provisioner?"
        log "btrfs: read-only subvolume snapshot $VOL_DIR -> $SNAP_DIR"
        btrfs subvolume snapshot -r "$VOL_DIR" "$SNAP_DIR"
        ;;
    xfs)
        command -v xfs_info >/dev/null 2>&1 || die "xfs_info not found (install xfsprogs)"
        mp=$(resolve_mountpoint "$(dirname "$VOL_DIR")")
        xfs_info "$mp" 2>/dev/null | grep -q 'reflink=1' \
            || die "snapshots not supported: xfs filesystem at $mp lacks reflink (reformat with mkfs.xfs -m reflink=1)"
        log "xfs: reflink clone $VOL_DIR -> $SNAP_DIR (workload should be quiesced)"
        cp --reflink=always -aR "$VOL_DIR" "$SNAP_DIR"
        chmod -R a-w "$SNAP_DIR" 2>/dev/null || true
        ;;
    *)
        die "snapshots not supported on filesystem $fs (need btrfs subvolumes or xfs reflink)"
        ;;
    esac
fi

# JSON sidecar — makes the snapshot self-describing from the node alone.
cat > "$sidecar" <<EOF
{
  "snapshotPath": "$SNAP_DIR",
  "sourcePath": "$VOL_DIR",
  "fsType": "$fs",
  "sizeBytes": ${VOL_SIZE_BYTES:-0},
  "creationTime": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
log "snapshot complete: $SNAP_DIR (fs=$fs)"
