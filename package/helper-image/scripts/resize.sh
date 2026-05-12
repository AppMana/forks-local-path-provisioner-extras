#!/bin/sh
# resize.sh — update the per-filesystem quota to a new size.
#
# Two modes:
#   resize        (default) — set the quota for VOL_DIR to VOL_SIZE_BYTES
#   check-usage             — print USAGE_BYTES=<n> (actual disk usage)
#
# The CSI controller dispatches this on a PVC capacity change. For
# filesystems without project quotas (or VOL_QUOTA_TYPE=none) it is a
# metadata-only no-op.
#
# env: VOL_DIR VOL_SIZE_BYTES VOL_QUOTA_TYPE
set -eu
. /opt/local-path-provisioner/common.sh

# The CSI controller appends "-p <path> -s <size> -m <mode> -a <action>" to
# every helper-pod command. We act on env vars, but honor "-a check-usage"
# (or a literal "check-usage" argument) to switch modes.
action=resize
prev=
for arg in "$@"; do
    case "$prev" in -a) action=$arg ;; esac
    case "$arg" in check-usage) action=check-usage ;; esac
    prev=$arg
done

if [ "$action" = "check-usage" ]; then
    usage=$(du -sb "$VOL_DIR" 2>/dev/null | awk '{print $1}')
    [ -n "$usage" ] || usage=0
    echo "USAGE_BYTES=$usage"
    exit 0
fi

[ -n "${VOL_DIR:-}" ] || die "VOL_DIR not set"
parent=$(dirname "$VOL_DIR")
name=$(basename "$VOL_DIR")
qtype=$(resolve_quota_type "$parent")

case "$qtype" in
none)
    log "resize: no quota enforcement, metadata-only ($VOL_DIR -> $VOL_SIZE_BYTES bytes)"
    ;;
xfs)
    command -v xfs_quota >/dev/null 2>&1 || die "xfs_quota not found (install xfsprogs)"
    require_mount_option "$parent" prjquota
    log "xfs: resize $name bhard=$VOL_SIZE_BYTES bytes"
    xfs_quota -x -c "limit -p bhard=$VOL_SIZE_BYTES $name" "$parent"
    xfs_quota -x -c "report -pbih" "$parent" || true
    ;;
ext4)
    command -v setquota >/dev/null 2>&1 || die "setquota not found (install quota)"
    require_mount_option "$parent" prjquota
    pid=$(lookup_project_id "$name")
    [ -n "$pid" ] || die "no project ID recorded for $name; was this volume created with quota enforcement?"
    mp=$(resolve_mountpoint "$parent")
    blocks=$(( (VOL_SIZE_BYTES + 1023) / 1024 ))
    log "ext4: resize project $name id $pid on $mp, hard=$VOL_SIZE_BYTES bytes"
    setquota -P "$pid" 0 "$blocks" 0 0 "$mp"
    repquota -P "$mp" 2>/dev/null | grep -E "^#?$pid" || true
    ;;
btrfs)
    command -v btrfs >/dev/null 2>&1 || die "btrfs not found (install btrfs-progs)"
    btrfs subvolume show "$VOL_DIR" >/dev/null 2>&1 || die "$VOL_DIR is not a btrfs subvolume"
    log "btrfs: resize $VOL_DIR qgroup limit $VOL_SIZE_BYTES bytes"
    btrfs qgroup limit "$VOL_SIZE_BYTES" "$VOL_DIR"
    btrfs qgroup show -re "$VOL_DIR" 2>/dev/null || true
    ;;
*)
    die "unhandled quota type: $qtype"
    ;;
esac

log "resize complete: $VOL_DIR -> $VOL_SIZE_BYTES bytes"
