# snapshot.ps1 - create or delete a read-only snapshot of a volume directory.
#
# Two modes (via the appended "-a <action>"):
#   -a snapshot          clone VOL_DIR -> SNAP_DIR + write a JSON sidecar
#   -a delete-snapshot   remove SNAP_DIR (and SNAP_DIR.json)
#
# env: VOL_DIR SNAP_DIR VOL_SIZE_BYTES VOL_QUOTA_TYPE
#
# Per-filesystem clone primitive:
#   ReFS : Copy-Item -Recurse   (block clone on same-volume ReFS >= 3.1 -
#          cheap, but per-file, not a point-in-time atomic snapshot)
#   NTFS : VSS shadow copy of the volume + robocopy the directory out
#          (point-in-time, crash-consistent). Falls back to a plain copy if
#          VSS is unavailable (e.g. client SKU, no Volume Shadow Copy service).

. C:\opt\local-path-csi-scripts\common.ps1

$action = 'snapshot'
for ($i = 0; $i -lt $args.Count; $i++) {
    if ($args[$i] -eq '-a' -and $i + 1 -lt $args.Count) { $action = $args[$i + 1] }
}

if (-not $env:SNAP_DIR) { Die "SNAP_DIR not set" }
$sidecar = "$($env:SNAP_DIR).json"

if ($action -eq 'delete-snapshot') {
    if (Test-Path $env:SNAP_DIR) { Remove-Item -Recurse -Force $env:SNAP_DIR }
    if (Test-Path $sidecar)      { Remove-Item -Force $sidecar }
    Write-Log "delete-snapshot complete: $($env:SNAP_DIR)"
    exit 0
}

# --- create ---
if (-not $env:VOL_DIR) { Die "VOL_DIR not set" }
if (-not (Test-Path $env:VOL_DIR)) { Die "source volume $($env:VOL_DIR) does not exist" }
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $env:SNAP_DIR) | Out-Null
$fs = Resolve-FsType $env:VOL_DIR

if (Test-Path $env:SNAP_DIR) {
    Write-Log "snapshot $($env:SNAP_DIR) already exists, treating as idempotent"
} else {
    switch ($fs) {
        'ReFS' {
            Write-Log "ReFS: block-clone copy $($env:VOL_DIR) -> $($env:SNAP_DIR)"
            Copy-Item -Recurse -Force $env:VOL_DIR $env:SNAP_DIR
        }
        'NTFS' {
            # VSS produces a point-in-time, crash-consistent shadow of the
            # source volume. The shadow's DeviceObject ("\\?\GLOBALROOT\Device\
            # HarddiskVolumeShadowCopyN") is a device-object path which many
            # Win32 callers (robocopy, Test-Path, File.Exists) handle
            # inconsistently and which is unstable across rapid create/delete
            # cycles. The canonical workaround documented by Microsoft is to
            # expose the shadow through a directory symbolic link (mklink /D)
            # and read through the link. Win32 path APIs follow the reparse
            # point correctly and the shadow's contents become visible.
            $done = $false
            $shadow = $null
            $mount = $null
            try {
                $drive = Resolve-VolumeDriveLetter $env:VOL_DIR
                if (-not $drive) { throw "could not resolve drive letter for $($env:VOL_DIR)" }
                Write-Log "NTFS: creating VSS shadow copy of ${drive}:\ for a point-in-time snapshot"
                $cls = [WMICLASS]"root\cimv2:Win32_ShadowCopy"
                $res = $cls.Create("${drive}:\", "ClientAccessible")
                if ($res.ReturnValue -ne 0) { throw "Win32_ShadowCopy.Create returned $($res.ReturnValue)" }
                $shadow = Get-CimInstance Win32_ShadowCopy | Where-Object { $_.ID -eq $res.ShadowID }
                if (-not $shadow) { throw "shadow copy $($res.ShadowID) not found after Create" }
                # mklink requires a trailing backslash on the target so it
                # treats the device-object path as a directory.
                $deviceTarget = $shadow.DeviceObject.TrimEnd('\') + '\'
                $mount = Join-Path $env:TEMP ("lpp-vss-" + [System.Guid]::NewGuid().ToString("N"))
                $mkOut = cmd /c "mklink /D `"$mount`" `"$deviceTarget`"" 2>&1
                if ($LASTEXITCODE -ne 0 -or -not (Test-Path $mount)) {
                    throw "mklink /D $mount -> $deviceTarget failed: $mkOut"
                }
                $rel = $env:VOL_DIR.Substring(3)  # strip "X:\"
                $src = Join-Path $mount $rel
                if (-not (Test-Path $src)) { throw "shadow does not contain $src" }
                Write-Log "NTFS: robocopy $src -> $($env:SNAP_DIR)"
                robocopy $src $env:SNAP_DIR /MIR /COPY:DAT /R:1 /W:1 /NFL /NDL /NJH /NJS | Out-Null
                # robocopy exit codes 0..7 are success-ish (files copied,
                # no-op, mismatched, extras), >= 8 is a real failure.
                if ($LASTEXITCODE -ge 8) { throw "robocopy failed with exit code $LASTEXITCODE" }
                $done = $true
            } catch {
                Write-Log "WARNING: NTFS VSS snapshot failed ($($_.Exception.Message)); falling back to a plain copy (not point-in-time)"
            } finally {
                # Drop the symlink first, then the shadow copy. Removing a
                # directory symbolic link via Remove-Item -Recurse deletes the
                # link target's contents on older PowerShell versions, so use
                # cmd /c rmdir which only removes the link.
                if ($mount -and (Test-Path $mount)) {
                    cmd /c "rmdir `"$mount`"" 2>&1 | Out-Null
                }
                if ($shadow) {
                    # CimInstance has no .Delete() method (that's the legacy
                    # ManagementObject API on Get-WmiObject); use
                    # Remove-CimInstance instead.
                    Remove-CimInstance -InputObject $shadow -ErrorAction SilentlyContinue
                }
            }
            if (-not $done) {
                Copy-Item -Recurse -Force $env:VOL_DIR $env:SNAP_DIR
            }
        }
        default { Die "snapshots not supported on filesystem $fs (need ReFS block clone or an NTFS volume with VSS)" }
    }
    Get-ChildItem -Recurse -Force $env:SNAP_DIR -ErrorAction SilentlyContinue | ForEach-Object {
        try { $_.IsReadOnly = $true } catch { }
    }
}

@"
{
  "snapshotPath": "$($env:SNAP_DIR)",
  "sourcePath": "$($env:VOL_DIR)",
  "fsType": "$fs",
  "sizeBytes": $([int64]($env:VOL_SIZE_BYTES)),
  "creationTime": "$(Get-Date -Format o)"
}
"@ | Set-Content -Path $sidecar -Encoding ASCII

Write-Log "snapshot complete: $($env:SNAP_DIR) (fs=$fs)"
