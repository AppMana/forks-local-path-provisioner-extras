#!/bin/sh
# teardown.sh — remove the per-filesystem quota and the volume directory.
#
# env: VOL_DIR VOL_QUOTA_TYPE
set -eu
. /usr/local/sbin/common.sh

[ -n "${VOL_DIR:-}" ] || die "VOL_DIR not set"
parent=$(dirname "$VOL_DIR")
name=$(basename "$VOL_DIR")

# On teardown, an unsupported / unknown FS just means "no quota to remove".
qtype=none
case "${VOL_QUOTA_TYPE:-none}" in
    none|"") qtype=none ;;
    xfs|ext4|btrfs) qtype=$VOL_QUOTA_TYPE ;;
    auto)
        case "$(resolve_fs "$parent")" in
            xfs) qtype=xfs ;; ext4) qtype=ext4 ;; btrfs) qtype=btrfs ;; *) qtype=none ;;
        esac
        ;;
    *) qtype=none ;;
esac

case "$qtype" in
xfs)
    if command -v xfs_quota >/dev/null 2>&1; then
        xfs_quota -x -c "limit -p bhard=0 $name" "$parent" 2>/dev/null || true
    fi
    rm -rf "$VOL_DIR"
    free_project_id "$name" "$VOL_DIR"
    ;;
ext4)
    pid=$(lookup_project_id "$name")
    mp=$(resolve_mountpoint "$parent")
    if [ -n "$pid" ] && command -v setquota >/dev/null 2>&1 && [ -n "$mp" ]; then
        setquota -P "$pid" 0 0 0 0 "$mp" 2>/dev/null || true
    fi
    rm -rf "$VOL_DIR"
    free_project_id "$name" "$VOL_DIR"
    ;;
btrfs)
    if command -v btrfs >/dev/null 2>&1 && btrfs subvolume show "$VOL_DIR" >/dev/null 2>&1; then
        # Deleting the subvolume drops its qgroup with it.
        btrfs subvolume delete "$VOL_DIR"
    else
        rm -rf "$VOL_DIR"
    fi
    ;;
none|"")
    rm -rf "$VOL_DIR"
    ;;
esac

log "teardown complete: $VOL_DIR"
