# syntax=docker/dockerfile:1.7.0
#
# local-path helper image (Linux). Built multi-arch (linux/amd64, linux/arm64)
# by .github/workflows/helper-image.yml. Contains the filesystem userspace
# tools the setup/teardown/resize scripts need:
#   - xfsprogs    : xfs_quota (xfs project quotas)
#   - quota       : setquota / repquota (ext4 project quotas)
#   - e2fsprogs   : chattr +P (bind a dir tree to an ext4 project)
#   - btrfs-progs : btrfs subvolume / qgroup
#   - util-linux  : findmnt, flock
#   - coreutils   : du, stat, etc.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xfsprogs \
      quota \
      e2fsprogs \
      btrfs-progs \
      util-linux \
      coreutils \
 && rm -rf /var/lib/apt/lists/*

# Scripts live in /usr/local/sbin, NOT /opt/local-path-provisioner.
# The helper pod mounts the volume's parent directory at /opt/...
# via hostPath, which would shadow scripts living under /opt.
COPY scripts/common.sh scripts/setup.sh scripts/teardown.sh scripts/resize.sh \
     scripts/snapshot.sh scripts/restore.sh \
     /usr/local/sbin/
RUN chmod 0755 /usr/local/sbin/setup.sh /usr/local/sbin/teardown.sh \
                /usr/local/sbin/resize.sh /usr/local/sbin/snapshot.sh \
                /usr/local/sbin/restore.sh /usr/local/sbin/common.sh

WORKDIR /
