# Snapshots — per-filesystem mechanics

The CSI driver exposes the standard `VolumeSnapshot` / `VolumeSnapshotContent`
API. Internally every snapshot operation is a one-shot helper-pod invocation
of the `snapshot` action with `VOL_DIR` (the source volume) and `SNAP_DIR`
(the destination) set. The default scripts pick the cheapest crash-consistent
clone primitive that the filesystem supports, and fall back to a plain
recursive copy when no faster primitive exists. A JSON sidecar
(`<SNAP_DIR>.json`) records `sourcePath`, `fsType`, `sizeBytes`, and the
creation time so `ListSnapshots` can reconstruct state across controller
restarts without inspecting on-disk inodes.

## Linux

### btrfs — `btrfs subvolume snapshot -r`

Atomic, copy-on-write, instantaneous. `restore` uses `btrfs subvolume
snapshot` (without `-r`) so the new volume is writable from byte zero. Per-FS
state lives entirely in the subvolume tree; nothing in `/etc/projects` to
clean up.

Requires `VOL_DIR` to already be a btrfs subvolume — the shipped `setup.sh`
creates one when it sees a btrfs parent, so this is automatic for volumes
provisioned by the driver. Volumes created by another tool (or `mkdir`ed
directly under a btrfs parent) are not subvolumes and the snapshot script
exits with a clear error rather than silently degrading to a copy.

### xfs — `cp --reflink=always`

Reflinks share blocks at the FS level until either side writes, so the
snapshot is cheap and crash-consistent (the reflink call is atomic from the
caller's perspective). Requires xfs `reflink=1` (the default since
xfsprogs 5.5 / kernel 5.1). If reflink is unavailable the script does not
fall back to a plain copy — it exits with an error so the operator notices.

### ext4 — recursive `cp -aR`

ext4 has no usable snapshot primitive: the `e4_snapshots` patch never
upstreamed, and reflink on ext4 (kernel 5.10+) is still marked experimental
and disabled by default. The default script does a plain recursive copy.
That's not point-in-time consistent if the volume is being written
concurrently — callers that need real PIT semantics on ext4 should quiesce
the workload or layer LVM2 thin snapshots on the host (out of scope for the
default script).

### Other (ntfs-3g, FAT, NFS, …) — recursive `cp -aR`

Same fallback. The snapshot succeeds, the sidecar records the FS, and the
operator gets a clear log line telling them it was a plain copy.

## Windows

### NTFS — VSS shadow + mklink + robocopy

NTFS is the interesting one. The shipped script takes a Volume Shadow Copy
of the underlying volume (`Win32_ShadowCopy.Create`), reads the source
directory through the shadow, and `robocopy /MIR` extracts that
point-in-time state into `SNAP_DIR`. Shadow creation and deletion are
millisecond-scale because VSS uses copy-on-write at the block layer.

#### The GLOBALROOT / mklink workaround

`Win32_ShadowCopy.DeviceObject` returns a path of the form
`\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopyN`. That's a valid Win32
device-object path on paper, but several Win32 path APIs handle it
inconsistently:

- robocopy intermittently returns `ERROR 53` ("The network path was not
  found") when asked to enumerate a directory inside it.
- `Test-Path` returns `False` for paths that demonstrably exist on the
  shadow.
- The failure mode is timing-sensitive: rapid create → robocopy → delete →
  recreate cycles reproduce it reliably; the same operations spaced out by
  seconds usually don't.

The fix that Microsoft has documented since at least Windows Server 2012 is
to expose the shadow through a directory symbolic link (`mklink /D`) and
read through the link. The link is a reparse point that Win32 path APIs
follow correctly, so robocopy enumerates the shadow's tree without issue.

In code:

```powershell
$mount = Join-Path $env:TEMP ("lpp-vss-" + [System.Guid]::NewGuid().ToString("N"))
cmd /c "mklink /D `"$mount`" `"$($shadow.DeviceObject.TrimEnd('\') + '\')`"" | Out-Null
robocopy (Join-Path $mount $rel) $dest /MIR /COPY:DAT /R:1 /W:1
cmd /c "rmdir `"$mount`""        # remove the link only, NOT its target
Remove-CimInstance -InputObject $shadow
```

Three things that look like bikeshedding but aren't:

1. **`mklink` target needs a trailing `\`.** Without it, mklink creates a
   file symlink and robocopy refuses to treat it as a directory.
2. **Remove the link with `cmd /c rmdir`, not `Remove-Item -Recurse`.** On
   older PowerShell builds `Remove-Item -Recurse` walks through the reparse
   point and deletes the link target's contents — i.e. the live filesystem
   under the shadow. `rmdir` deletes only the link.
3. **`Remove-CimInstance`, not `$shadow.Delete()`.** `Get-CimInstance`
   returns a `Microsoft.Management.Infrastructure.CimInstance`. It does not
   have a `Delete()` method (that's the legacy `ManagementObject` returned
   by `Get-WmiObject`). Calling `.Delete()` throws and leaks the shadow.

Verified on the qemu Windows Server 2022 lab VM (build 20348) with three
back-to-back create/delete cycles and a point-in-time semantics check
(snapshot before modify → restore reproduces the pre-modify state). Zero
leaked shadows after each cycle.

#### Fallback

If VSS fails for an environment-specific reason (Volume Shadow Copy service
disabled, no free shadow storage), the script falls back to a plain
`Copy-Item -Recurse` of the live directory. The sidecar still records
`fsType: NTFS` and the snapshot succeeds, but the operator sees a
`WARNING: NTFS VSS snapshot failed (...); falling back to a plain copy
(not point-in-time)` log line.

### ReFS — recursive `Copy-Item`

ReFS 3.1+ implements block clone via `FSCTL_DUPLICATE_EXTENTS_TO_FILE`. The
Win32 `CopyFileEx` API uses that ioctl transparently when source and
destination live on the same ReFS volume, so a plain `Copy-Item -Recurse`
gives you a cheap block-cloned copy without any extra plumbing. That's
file-level CoW, not the volume-level point-in-time of VSS — concurrent
writes to the source between `Copy-Item` starting and finishing can leak
into the snapshot — but it matches the cost model of btrfs/xfs reflinks on
Linux.

FSRM quotas don't apply on ReFS (Windows blocks `New-FsrmQuota` on ReFS
paths), so the snapshot script doesn't have to wrangle quota state on the
destination.

## Sidecar

For every snapshot the script writes `<SNAP_DIR>.json`:

```json
{
  "snapshotPath": "C:\\opt\\local-path-provisioner\\.snapshots\\snap-foo",
  "sourcePath":   "C:\\opt\\local-path-provisioner\\pvc-foo_default_data",
  "fsType":       "NTFS",
  "sizeBytes":    1073741824,
  "creationTime": "2026-05-13T14:21:21.3908061-07:00"
}
```

The CSI controller's `ListSnapshots` rebuilds its in-memory index from these
sidecars on startup, so deleting `SNAP_DIR` without deleting the sidecar
leaves the controller in an inconsistent state. The `delete-snapshot` action
removes both.
