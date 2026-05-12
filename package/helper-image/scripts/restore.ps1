# restore.ps1 - create a new read-write volume directory from a snapshot, then
# apply the requested FSRM quota.
#
# env: SNAP_DIR (source) VOL_DIR (new volume) VOL_SIZE_BYTES VOL_QUOTA_TYPE

. C:\opt\local-path-provisioner\common.ps1

if (-not $env:SNAP_DIR) { Die "SNAP_DIR not set" }
if (-not $env:VOL_DIR)  { Die "VOL_DIR not set" }
if (-not (Test-Path $env:SNAP_DIR)) { Die "snapshot $($env:SNAP_DIR) does not exist" }
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $env:VOL_DIR) | Out-Null

if (Test-Path $env:VOL_DIR) {
    Write-Log "destination $($env:VOL_DIR) already exists, treating restore as idempotent"
} else {
    $fs = Resolve-FsType (Split-Path -Parent $env:VOL_DIR)
    # ReFS Copy-Item is a block clone on the same volume; NTFS is a plain copy.
    Write-Log "${fs}: copy $($env:SNAP_DIR) -> $($env:VOL_DIR)"
    Copy-Item -Recurse -Force $env:SNAP_DIR $env:VOL_DIR
    Get-ChildItem -Recurse -Force $env:VOL_DIR -ErrorAction SilentlyContinue | ForEach-Object {
        try { $_.IsReadOnly = $false } catch { }
    }
}

$qtype = Resolve-QuotaType (Split-Path -Parent $env:VOL_DIR)
switch ($qtype) {
    'none' { Write-Log "no quota enforcement on restored volume $($env:VOL_DIR)" }
    { $_ -in 'ntfs', 'refs' } {
        $size = [int64]$env:VOL_SIZE_BYTES
        Remove-FsrmHardQuota $env:VOL_DIR
        Set-FsrmHardQuota $env:VOL_DIR $size
    }
}

Write-Log "restore complete: $($env:VOL_DIR) (from $($env:SNAP_DIR))"
