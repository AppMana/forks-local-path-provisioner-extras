# setup.ps1 — create the volume directory and apply an FSRM hard quota.
#
# env: VOL_DIR VOL_SIZE_BYTES VOL_MODE VOL_QUOTA_TYPE
#
# Full enforcement (auto-extend, error reporting, ReFS specifics) lands in M6.

. C:\opt\local-path-provisioner\common.ps1

if (-not $env:VOL_DIR) { Throw-Die "VOL_DIR not set" }
New-Item -ItemType Directory -Force -Path $env:VOL_DIR | Out-Null

if ($env:VOL_MODE -eq 'Block') {
    Write-Log "Block mode volume — quota enforcement not applicable, directory created"
    exit 0
}

$qtype = Resolve-QuotaType $env:VOL_DIR
switch ($qtype) {
    'none' { Write-Log "no quota enforcement requested, directory ready: $($env:VOL_DIR)" }
    { $_ -in 'ntfs', 'refs' } {
        Ensure-FsrmAvailable
        $size = [int64]$env:VOL_SIZE_BYTES
        Write-Log "$qtype: FSRM hard quota $size bytes on $($env:VOL_DIR)"
        # Idempotent: remove any stale quota at this path first.
        Get-FsrmQuota -Path $env:VOL_DIR -ErrorAction SilentlyContinue | Remove-FsrmQuota -Confirm:$false -ErrorAction SilentlyContinue
        New-FsrmQuota -Path $env:VOL_DIR -Size $size -Description "local-path-provisioner" | Out-Null
        Get-FsrmQuota -Path $env:VOL_DIR | Format-List Path, Size, Usage
    }
    default { Throw-Die "unhandled quota type: $qtype" }
}

Write-Log "setup complete: $($env:VOL_DIR)"
