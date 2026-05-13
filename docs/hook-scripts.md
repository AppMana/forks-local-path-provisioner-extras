# Hook scripts — contract and defaults

The CSI controller dispatches every volume lifecycle operation by running a
short-lived helper pod on the target node. That pod's container command is a
**hook script**. The contract below is the same for every action, every OS, and
every filesystem, so end users can drop in their own scripts without having to
rediscover what's expected. The scripts shipped in
`package/helper-image/scripts/` are the defaults — they cover NTFS, ReFS,
xfs, ext4, and btrfs robustly, and you only need to replace one when you want
behavior that's specific to your environment.

## Actions

Every operation maps to one of six action names. The controller emits the
action name both as the `-a <action>` argument and (implicitly) by selecting a
script via the override matrix below.

| Action            | When the controller runs it                                  | Required env             |
|-------------------|--------------------------------------------------------------|--------------------------|
| `create`          | `CreateVolume` — provision an empty volume                   | `VOL_DIR`, `VOL_SIZE_BYTES`, `VOL_MODE`, `VOL_QUOTA_TYPE` |
| `delete`          | `DeleteVolume` — release the volume dir and any quota state  | `VOL_DIR`, `VOL_QUOTA_TYPE` |
| `resize`          | `ControllerExpandVolume` — update the byte budget            | `VOL_DIR`, `VOL_SIZE_BYTES`, `VOL_QUOTA_TYPE` |
| `check-usage`     | `ControllerExpandVolume` shrink pre-check — print bytes used | `VOL_DIR` (read), bytes on stdout |
| `snapshot`        | `CreateSnapshot` — clone `VOL_DIR` to `SNAP_DIR`             | `VOL_DIR`, `SNAP_DIR`, `VOL_SIZE_BYTES`, `VOL_QUOTA_TYPE` |
| `delete-snapshot` | `DeleteSnapshot` — remove `SNAP_DIR` and its sidecar         | `SNAP_DIR` |
| `restore`         | `CreateVolume` from a snapshot source                        | `SNAP_DIR`, `VOL_DIR`, `VOL_SIZE_BYTES`, `VOL_QUOTA_TYPE` |

`snapshot` and `delete-snapshot` share a single command by convention; the
script branches on `-a`. The shipped `snapshot.sh` and `snapshot.ps1` follow
that pattern.

## Environment variables

The controller sets these on the helper-pod container before invoking the
command. Custom scripts MUST treat unset / empty values as fatal where the
table above lists the var as required.

| Variable          | Value                                                                 |
|-------------------|-----------------------------------------------------------------------|
| `VOL_DIR`         | Absolute path of the volume directory in node-OS form. Linux: `/opt/local-path-provisioner/pvc-X_default_data`. Windows: `C:\opt\local-path-provisioner\pvc-X_default_data`. |
| `VOL_SIZE_BYTES`  | Decimal int64 byte budget for the volume. Zero means "metadata only, no enforcement". |
| `VOL_MODE`        | `Filesystem` or `Block`. Block mode tells the script to provision a sparse file backing instead of a directory. |
| `VOL_QUOTA_TYPE`  | One of `auto`, `none`, `xfs`, `ext4`, `btrfs`, `ntfs`, `refs`. `auto` means "detect from the filesystem at `VOL_DIR`". Custom scripts that don't enforce quotas may ignore it. |
| `SNAP_DIR`        | Absolute path of the snapshot directory in node-OS form. Only set for snapshot/restore/delete-snapshot. |

## Command-line arguments

Every helper-pod command receives, in order:

```
-p <VOL_DIR> -s <VOL_SIZE_BYTES> -m <VOL_MODE> -a <action>
```

These are redundant with the env vars and exist for scripts that prefer
argument parsing (and for `-a`, which is the canonical action selector for
scripts that handle multiple actions). The shipped scripts read env vars and
only parse `-a` when they need to distinguish actions.

## Stdout / stderr / exit codes

- **Exit 0** is success. Any non-zero exit fails the CSI operation and the
  helper pod's logs are surfaced in the controller's error message.
- **stderr** is for human-readable progress and warnings. The shipped scripts
  log via `log()` in `common.sh` (which writes to stdout) and `Write-Log` in
  `common.ps1` (which writes to stderr — PowerShell's stdout is captured into
  function return values, so logging there corrupts `$(Resolve-...)` returns).
- **stdout** is reserved for action-specific structured output. Today that's
  one case: `check-usage` MUST print a single decimal byte count followed by a
  newline. Everything else should write to stderr to leave stdout for future
  structured contracts.

## Override matrix

The controller resolves the command for `(action, OS)` in this order, picking
the first non-empty match:

1. Per-StorageClass override (planned; see `StorageClassConfigs.<name>.<cmd>` —
   currently only the global override is wired)
2. Per-OS override for the action — `windows.snapshotCommand`, etc.
3. Global override for the action — `snapshotCommand`, etc.
4. Built-in default for the action and the resolved OS

The target OS is resolved at dispatch time from the node's `kubernetes.io/os`
label. Linux is the implicit default for unlabelled nodes.

### config.json schema (excerpt)

```jsonc
{
  // global defaults for any node, used when no per-OS or per-SC override exists
  "setupCommand":    "/usr/local/sbin/setup.sh",
  "teardownCommand": "/usr/local/sbin/teardown.sh",
  "resizeCommand":   "/usr/local/sbin/resize.sh",
  "snapshotCommand": "/usr/local/sbin/snapshot.sh",
  "restoreCommand":  "/usr/local/sbin/restore.sh",

  // per-OS overrides used when kubernetes.io/os == windows
  "windows": {
    "setupCommand":    ["powershell", "-NoProfile", "-File", "C:\\opt\\local-path-csi-scripts\\setup.ps1"],
    "teardownCommand": ["powershell", "-NoProfile", "-File", "C:\\opt\\local-path-csi-scripts\\teardown.ps1"],
    "resizeCommand":   ["powershell", "-NoProfile", "-File", "C:\\opt\\local-path-csi-scripts\\resize.ps1"],
    "snapshotCommand": ["powershell", "-NoProfile", "-File", "C:\\opt\\local-path-csi-scripts\\snapshot.ps1"],
    "restoreCommand":  ["powershell", "-NoProfile", "-File", "C:\\opt\\local-path-csi-scripts\\restore.ps1"]
  }
}
```

Windows entries accept either a single string (whitespace-tokenized) or an
array of argv tokens (preferred — handles paths containing spaces).

### Replacing a single hook

To swap out only the `snapshot` action on Linux, override `snapshotCommand`
in the ConfigMap:

```yaml
config.json: |
  {
    "nodePathMap": [{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/opt/local-path-provisioner"]}],
    "snapshotCommand": "/opt/my-snapshot-hook.sh"
  }
```

Make sure `/opt/my-snapshot-hook.sh` is reachable inside the helper pod (bake
it into a forked helper image or mount it from a ConfigMap volume on the pod
template). All other actions continue to use the shipped defaults.

### Authoring a custom script — checklist

1. Read the required env vars from the action table; fail fast if any are
   empty.
2. Treat `VOL_DIR` as an absolute path in the **node's** path style (mixed
   forward/back slashes on Windows are tolerated). Do not assume Linux paths
   on a Windows hook.
3. Honor `VOL_QUOTA_TYPE=none` by skipping enforcement and exiting 0; this is
   the value the controller emits when the operator has explicitly opted out.
4. Make the script idempotent. The controller retries on transient failures;
   re-running a successful action MUST be a no-op (don't fail because the
   target already exists).
5. For `check-usage`, print only the byte count to stdout. The controller
   parses the first decimal it sees and rejects shrink requests where
   `usage > VOL_SIZE_BYTES`.

## Built-in defaults

### Linux (`/usr/local/sbin/<action>.sh`)

| Filesystem | `create`                          | `resize`                                    | `snapshot`                | `restore`                     |
|------------|-----------------------------------|----------------------------------------------|---------------------------|--------------------------------|
| xfs        | `mkdir 0777` + project quota      | `xfs_quota limit -p bhard=N`                 | `cp --reflink=always`     | `cp --reflink=always`         |
| ext4       | `mkdir 0777` + `chattr +P` + project quota | `setquota -P` (blocks = ceil(N/1024)) | recursive `cp -aR` (no reflink) | recursive `cp -aR` |
| btrfs      | `btrfs subvolume create` + qgroup | `btrfs qgroup limit N`                       | `btrfs subvolume snapshot -r` | `btrfs subvolume snapshot` |
| other      | `mkdir 0777`, no quota             | metadata only                                | recursive `cp -aR`        | recursive `cp -aR`            |

Block mode (`VOL_MODE=Block`) provisions a sparse file at
`$VOL_DIR/disk.img` sized to `VOL_SIZE_BYTES` and is FS-independent.

### Windows (`powershell -NoProfile -File C:\opt\local-path-csi-scripts\<action>.ps1`)

| Filesystem | `create`                                | `resize`                              | `snapshot`                              | `restore`             |
|------------|-----------------------------------------|----------------------------------------|------------------------------------------|------------------------|
| NTFS       | `New-Item -ItemType Directory` + FSRM hard quota | `Set-FsrmQuota -Size N -SoftLimit:$false` | VSS shadow + mklink + `robocopy /MIR` | recursive `Copy-Item` |
| ReFS       | `New-Item -ItemType Directory` (no FSRM — Windows blocks it) | metadata only | recursive `Copy-Item` (block clone on ReFS ≥ 3.1) | recursive `Copy-Item` |

See [`snapshots.md`](snapshots.md) for the why behind the Windows VSS+mklink
detour and [`windows-fsrm.md`](windows-fsrm.md) for FSRM's silent
soft-quota fallback on system paths.

## Helper-pod template

The command runs inside the helper-pod template registered in the
`local-path-config` ConfigMap (`helperPod.yaml` for Linux,
`helperPod-windows.yaml` for Windows). A custom hook script's runtime
dependencies (binaries, mount points, capabilities) must be satisfied by that
template — the controller does not synthesize mounts based on which script it
dispatches.

If a custom script needs additional hostPath mounts or capabilities, fork the
helper image (or replace it via `--helper-image`) and edit the template in
the ConfigMap. The override matrix above is for swapping commands; the
template controls what the commands can see.
