#!/bin/sh
# setup.sh — create the volume directory and apply per-filesystem quota.
#
# env: VOL_DIR VOL_SIZE_BYTES VOL_MODE VOL_QUOTA_TYPE
set -eu
. /opt/local-path-provisioner/common.sh

[ -n "${VOL_DIR:-}" ] || die "VOL_DIR not set"
parent=$(dirname "$VOL_DIR")
name=$(basename "$VOL_DIR")
qtype=$(resolve_quota_type "$parent")

if [ "$qtype" = "btrfs" ]; then
    # btrfs: the volume directory must be a subvolume so it gets its own qgroup.
    if [ -d "$VOL_DIR" ]; then
        # Pre-created by an earlier (failed) attempt — only OK if it's already a subvolume.
        if ! btrfs subvolume show "$VOL_DIR" >/dev/null 2>&1; then
            die "$VOL_DIR exists but is not a btrfs subvolume; remove it and retry"
        fi
    else
        btrfs subvolume create "$VOL_DIR"
        chmod 0777 "$VOL_DIR"
    fi
else
    mkdir -m 0777 -p "$VOL_DIR"
fi

if [ "$VOL_MODE" = "Block" ]; then
    log "Block mode volume — quota enforcement is not applicable, directory created"
    exit 0
fi

case "$qtype" in
none)
    log "no quota enforcement requested, directory ready: $VOL_DIR"
    ;;
xfs)
    command -v xfs_quota >/dev/null 2>&1 || die "xfs_quota not found (install xfsprogs)"
    require_mount_option "$parent" prjquota
    pid=$(alloc_project_id "$VOL_DIR" "$name")
    log "xfs: project $name id $pid, bhard=$VOL_SIZE_BYTES bytes"
    xfs_quota -x -c "project -s $name" "$parent"
    xfs_quota -x -c "limit -p bhard=$VOL_SIZE_BYTES $name" "$parent"
    xfs_quota -x -c "report -pbih" "$parent" || true
    ;;
ext4)
    command -v setquota >/dev/null 2>&1 || die "setquota not found (install quota)"
    command -v chattr   >/dev/null 2>&1 || die "chattr not found (install e2fsprogs)"
    require_mount_option "$parent" prjquota
    mp=$(resolve_mountpoint "$parent")
    pid=$(alloc_project_id "$VOL_DIR" "$name")
    log "ext4: project $name id $pid on $mp, hard=$VOL_SIZE_BYTES bytes"
    # Bind the directory tree to the project ID (inherits to children via +P).
    chattr +P -p "$pid" "$VOL_DIR"
    # setquota -P takes block limits in KiB by default; pass byte limits via the
    # -b flag (block hard limit) using 1KiB blocks => ceil(bytes/1024).
    blocks=$(( (VOL_SIZE_BYTES + 1023) / 1024 ))
    setquota -P "$pid" 0 "$blocks" 0 0 "$mp"
    repquota -P "$mp" 2>/dev/null | grep -E "^#?$pid" || true
    ;;
btrfs)
    command -v btrfs >/dev/null 2>&1 || die "btrfs not found (install btrfs-progs)"
    mp=$(resolve_mountpoint "$parent")
    # Enabling quota is idempotent; ignore the "already enabled" error.
    btrfs quota enable "$mp" 2>/dev/null || true
    # Rescan so the just-created subvolume gets a qgroup. Best-effort.
    btrfs quota rescan -w "$mp" >/dev/null 2>&1 || true
    log "btrfs: subvolume $VOL_DIR qgroup limit $VOL_SIZE_BYTES bytes"
    btrfs qgroup limit "$VOL_SIZE_BYTES" "$VOL_DIR"
    btrfs qgroup show -re "$VOL_DIR" 2>/dev/null || true
    ;;
*)
    die "unhandled quota type: $qtype"
    ;;
esac

log "setup complete: $VOL_DIR"
