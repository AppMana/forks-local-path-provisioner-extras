#!/bin/sh
# common.sh — shared helpers for the local-path helper scripts.
#
# Every helper script receives these env vars from the CSI controller:
#   VOL_DIR          absolute path of the volume directory
#   VOL_SIZE_BYTES   requested size in bytes
#   VOL_MODE         Filesystem | Block
#   VOL_QUOTA_TYPE   auto | xfs | ext4 | btrfs | ntfs | refs | none | ""
#
# It is sourced (`. /opt/local-path-provisioner/common.sh`) by setup/teardown/resize.

set -eu

# Files shared by the xfs and ext4 project-quota branches. Mounted into the
# helper pod from the host (see the helperPod.yaml in the ConfigMap).
PROJECTS_FILE=/etc/projects
PROJID_FILE=/etc/projid
QUOTA_LOCKFILE=/var/lock/local-path-quota.lock

# log writes a timestamped line to STDERR. It must not go to stdout because
# several helpers (resolve_quota_type, alloc_project_id, ...) return their
# value via stdout and the caller does `x=$(helper ...)` — stdout logging
# would corrupt the captured value. The kubelet merges the helper pod's
# stdout+stderr into the container log, so the controller still sees these.
log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }

die() { log "ERROR: $*"; exit 1; }

# resolve_fs prints the filesystem type of the filesystem holding $1.
# Uses findmnt (util-linux) rather than `stat -f -c %T` because the latter
# reports ext4 as "ext2/ext3" on most kernels.
resolve_fs() {
    _target=$1
    # findmnt --target walks up to the mount point; -n no header; -o FSTYPE only.
    findmnt -n -o FSTYPE --target "$_target" 2>/dev/null || echo unknown
}

# resolve_mountpoint prints the mount point of the filesystem holding $1.
resolve_mountpoint() {
    findmnt -n -o TARGET --target "$1" 2>/dev/null || echo ""
}

# resolve_quota_type maps VOL_QUOTA_TYPE to a concrete type, auto-detecting
# from the filesystem when "auto". Echoes one of: xfs ext4 btrfs none.
# Exits non-zero on an unsupported FS when the request was explicit.
resolve_quota_type() {
    _parent=$1
    _requested=${VOL_QUOTA_TYPE:-none}
    case "$_requested" in
        none|"") echo none; return 0 ;;
        xfs|ext4|btrfs) echo "$_requested"; return 0 ;;
        ntfs|refs) die "quota type $_requested requested on a Linux node" ;;
        auto)
            _fs=$(resolve_fs "$_parent")
            case "$_fs" in
                xfs)   echo xfs ;;
                ext4)  echo ext4 ;;
                btrfs) echo btrfs ;;
                ext2|ext3)
                    # ext2/ext3 don't support project quotas; treat as none.
                    log "auto-detect: $_fs has no project-quota support, skipping enforcement"
                    echo none
                    ;;
                *)
                    log "auto-detect: filesystem $_fs has no quota support, skipping enforcement"
                    echo none
                    ;;
            esac
            ;;
        *) die "unknown VOL_QUOTA_TYPE: $_requested" ;;
    esac
}

# require_mount_option fails unless the mount point holding $1 carries option
# $2 (e.g. prjquota). Used by the xfs and ext4 branches.
require_mount_option() {
    _path=$1; _opt=$2
    _mp=$(resolve_mountpoint "$_path")
    [ -n "$_mp" ] || die "could not determine mount point for $_path"
    _opts=$(findmnt -n -o OPTIONS --target "$_path" 2>/dev/null || echo "")
    case ",$_opts," in
        *,"$_opt",*) : ;;
        *) die "filesystem at $_mp is not mounted with the $_opt option (current: $_opts); remount with: mount -o remount,$_opt $_mp" ;;
    esac
}

# alloc_project_id allocates a fresh project ID under flock and appends entries
# to /etc/projects and /etc/projid. Echoes the project ID. Used by xfs + ext4.
# Args: $1 = volume dir, $2 = project name (basename of the volume dir).
alloc_project_id() {
    _voldir=$1; _name=$2
    [ -w "$PROJECTS_FILE" ] || die "$PROJECTS_FILE is not writable (mount it read-write into the helper pod)"
    [ -w "$PROJID_FILE" ]   || die "$PROJID_FILE is not writable (mount it read-write into the helper pod)"
    (
        flock -x 9
        _max=$(awk -F: 'NF>=2{print $2}' "$PROJID_FILE" 2>/dev/null | sort -n | tail -1)
        if [ -z "$_max" ] || [ "$_max" -lt 10000 ]; then
            _id=10000
        else
            _id=$((_max + 1))
        fi
        printf '%s:%s\n' "$_id" "$_voldir" >> "$PROJECTS_FILE"
        printf '%s:%s\n' "$_name" "$_id"   >> "$PROJID_FILE"
        printf '%s' "$_id" > /tmp/.lpp_project_id
    ) 9>"$QUOTA_LOCKFILE"
    cat /tmp/.lpp_project_id
}

# lookup_project_id echoes the existing project ID for project name $1, or "".
lookup_project_id() {
    awk -F: -v n="$1" 'NF>=2 && $1==n {print $2; exit}' "$PROJID_FILE" 2>/dev/null || true
}

# free_project_id removes the project entries for name $1 / volume dir $2 from
# /etc/projid and /etc/projects under flock. sed -i needs write access to the
# directory for its temp file, so we use grep -v + cat instead.
free_project_id() {
    _name=$1; _voldir=$2
    (
        flock -x 9
        if grep -v "^${_name}:" "$PROJID_FILE" > /tmp/.lpp_projid 2>/dev/null; then
            cat /tmp/.lpp_projid > "$PROJID_FILE"
        fi
        rm -f /tmp/.lpp_projid
        if grep -v ":${_voldir}\$" "$PROJECTS_FILE" > /tmp/.lpp_projects 2>/dev/null; then
            cat /tmp/.lpp_projects > "$PROJECTS_FILE"
        fi
        rm -f /tmp/.lpp_projects
    ) 9>"$QUOTA_LOCKFILE"
}

# apply_quota applies a per-filesystem hard byte quota to an existing volume
# directory. Args: $1 = volume dir, $2 = size in bytes, $3 = quota type
# (xfs|ext4|btrfs|none). Used by setup.sh and restore.sh so the two paths
# stay in lockstep.
apply_quota() {
    _voldir=$1; _size=$2; _qtype=$3
    _parent=$(dirname "$_voldir")
    _name=$(basename "$_voldir")
    case "$_qtype" in
    none|"")
        log "no quota enforcement for $_voldir"
        ;;
    xfs)
        command -v xfs_quota >/dev/null 2>&1 || die "xfs_quota not found (install xfsprogs)"
        require_mount_option "$_parent" prjquota
        _pid=$(alloc_project_id "$_voldir" "$_name")
        log "xfs: project $_name id $_pid, bhard=$_size bytes"
        xfs_quota -x -c "project -s $_name" "$_parent"
        xfs_quota -x -c "limit -p bhard=$_size $_name" "$_parent"
        xfs_quota -x -c "report -pbih" "$_parent" || true
        ;;
    ext4)
        command -v setquota >/dev/null 2>&1 || die "setquota not found (install quota)"
        command -v chattr   >/dev/null 2>&1 || die "chattr not found (install e2fsprogs)"
        require_mount_option "$_parent" prjquota
        _mp=$(resolve_mountpoint "$_parent")
        _pid=$(alloc_project_id "$_voldir" "$_name")
        log "ext4: project $_name id $_pid on $_mp, hard=$_size bytes"
        chattr +P -p "$_pid" "$_voldir"
        _blocks=$(( (_size + 1023) / 1024 ))
        setquota -P "$_pid" 0 "$_blocks" 0 0 "$_mp"
        repquota -P "$_mp" 2>/dev/null | grep -E "^#?$_pid" || true
        ;;
    btrfs)
        command -v btrfs >/dev/null 2>&1 || die "btrfs not found (install btrfs-progs)"
        _mp=$(resolve_mountpoint "$_parent")
        btrfs quota enable "$_mp" 2>/dev/null || true
        btrfs quota rescan -w "$_mp" >/dev/null 2>&1 || true
        log "btrfs: subvolume $_voldir qgroup limit $_size bytes"
        btrfs qgroup limit "$_size" "$_voldir"
        btrfs qgroup show -re "$_voldir" 2>/dev/null || true
        ;;
    *)
        die "unhandled quota type: $_qtype"
        ;;
    esac
}
