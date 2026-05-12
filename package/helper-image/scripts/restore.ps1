# restore.ps1 — create a new read-write volume directory from a snapshot, then
# apply the requested FSRM quota.
#
# env: SNAP_DIR (source) VOL_DIR (new volume) VOL_SIZE_BYTES VOL_QUOTA_TYPE
#
# Windows skeleton — full ReFS block-clone / FSRM-on-restore is M6.

. C:\opt\local-path-provisioner\common.ps1

if (-not $env:SNAP_DIR) { Throw-Die "SNAP_DIR not set" }
if (-not $env:VOL_DIR)  { Throw-Die "VOL_DIR not set" }
if (-not (Test-Path $env:SNAP_DIR)) { Throw-Die "snapshot $($env:SNAP_DIR) does not exist" }
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $env:VOL_DIR) | Out-Null

if (Test-Path $env:VOL_DIR) {
    Write-Log "destination $($env:VOL_DIR) already exists, treating restore as idempotent"
} else {
    $fs = Resolve-FsType (Split-Path -Parent $env:VOL_DIR)
    Write-Log "$fs: copy $($env:SNAP_DIR) -> $($env:VOL_DIR)"
    Copy-Item -Recurse -Force $env:SNAP_DIR $env:VOL_DIR
    Get-ChildItem -Recurse $env:VOL_DIR | ForEach-Object { $_.IsReadOnly = $false } 2>$null
}

$qtype = Resolve-QuotaType (Split-Path -Parent $env:VOL_DIR)
if ($qtype -in 'ntfs', 'refs') {
    Ensure-FsrmAvailable
    $size = [int64]$env:VOL_SIZE_BYTES
    Get-FsrmQuota -Path $env:VOL_DIR -ErrorAction SilentlyContinue | Remove-FsrmQuota -Confirm:$false -ErrorAction SilentlyContinue
    New-FsrmQuota -Path $env:VOL_DIR -Size $size -Description "local-path-provisioner" | Out-Null
}

Write-Log "restore complete: $($env:VOL_DIR) (from $($env:SNAP_DIR))"
