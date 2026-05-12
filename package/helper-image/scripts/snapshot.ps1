# snapshot.ps1 — create or delete a read-only snapshot of a volume directory.
#
# Two modes (via "-a <action>"):
#   -a snapshot          ReFS block-clone (or VSS extract on NTFS) -> SNAP_DIR
#   -a delete-snapshot   remove SNAP_DIR (and SNAP_DIR.json)
#
# env: VOL_DIR SNAP_DIR VOL_SIZE_BYTES VOL_QUOTA_TYPE
#
# NOTE: this is the Windows skeleton. Full ReFS block-clone / NTFS VSS-extract
# implementation lands in M6. For now ReFS uses Copy-Item (auto block-clone on
# same-volume ReFS >= 3.1); NTFS falls back to a plain copy.

. C:\opt\local-path-provisioner\common.ps1

$action = 'snapshot'
for ($i = 0; $i -lt $args.Count; $i++) {
    if ($args[$i] -eq '-a' -and $i + 1 -lt $args.Count) { $action = $args[$i + 1] }
}

if (-not $env:SNAP_DIR) { Throw-Die "SNAP_DIR not set" }
$sidecar = "$($env:SNAP_DIR).json"

if ($action -eq 'delete-snapshot') {
    if (Test-Path $env:SNAP_DIR) { Remove-Item -Recurse -Force $env:SNAP_DIR }
    if (Test-Path $sidecar)      { Remove-Item -Force $sidecar }
    Write-Log "delete-snapshot complete: $($env:SNAP_DIR)"
    exit 0
}

if (-not $env:VOL_DIR) { Throw-Die "VOL_DIR not set" }
if (-not (Test-Path $env:VOL_DIR)) { Throw-Die "source volume $($env:VOL_DIR) does not exist" }
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
            # M6: replace with a VSS shadow-copy extract for point-in-time consistency.
            Write-Log "NTFS: plain copy $($env:VOL_DIR) -> $($env:SNAP_DIR) (not point-in-time; VSS extract is M6)"
            Copy-Item -Recurse -Force $env:VOL_DIR $env:SNAP_DIR
        }
        default { Throw-Die "snapshots not supported on filesystem $fs" }
    }
    Get-ChildItem -Recurse $env:SNAP_DIR | ForEach-Object { $_.IsReadOnly = $true } 2>$null
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
