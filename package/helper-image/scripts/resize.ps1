# resize.ps1 — update the FSRM hard quota to a new size.
#
# Two modes:
#   resize        (default) — set the quota for VOL_DIR to VOL_SIZE_BYTES
#   check-usage             — print USAGE_BYTES=<n>
#
# env: VOL_DIR VOL_SIZE_BYTES VOL_QUOTA_TYPE

. C:\opt\local-path-provisioner\common.ps1

# The CSI controller appends "-p <path> -s <size> -m <mode> -a <action>". We
# act on env vars, but honor "-a check-usage" (or a literal "check-usage").
$action = 'resize'
for ($i = 0; $i -lt $args.Count; $i++) {
    if ($args[$i] -eq '-a' -and $i + 1 -lt $args.Count) { $action = $args[$i + 1] }
    if ($args[$i] -eq 'check-usage') { $action = 'check-usage' }
}

if ($action -eq 'check-usage') {
    $bytes = 0
    if (Test-Path $env:VOL_DIR) {
        $bytes = (Get-ChildItem -Recurse -Force -File $env:VOL_DIR -ErrorAction SilentlyContinue |
                  Measure-Object -Property Length -Sum).Sum
        if (-not $bytes) { $bytes = 0 }
    }
    Write-Output "USAGE_BYTES=$bytes"
    exit 0
}

if (-not $env:VOL_DIR) { Throw-Die "VOL_DIR not set" }
$qtype = Resolve-QuotaType $env:VOL_DIR

switch ($qtype) {
    'none' { Write-Log "resize: no quota enforcement, metadata-only ($($env:VOL_DIR) -> $($env:VOL_SIZE_BYTES) bytes)" }
    { $_ -in 'ntfs', 'refs' } {
        Ensure-FsrmAvailable
        $size = [int64]$env:VOL_SIZE_BYTES
        $existing = Get-FsrmQuota -Path $env:VOL_DIR -ErrorAction SilentlyContinue
        if (-not $existing) { Throw-Die "no FSRM quota at $($env:VOL_DIR); was this volume created with quota enforcement?" }
        Write-Log "$qtype: resize FSRM quota $($env:VOL_DIR) -> $size bytes"
        Set-FsrmQuota -Path $env:VOL_DIR -Size $size | Out-Null
        Get-FsrmQuota -Path $env:VOL_DIR | Format-List Path, Size, Usage
    }
    default { Throw-Die "unhandled quota type: $qtype" }
}

Write-Log "resize complete: $($env:VOL_DIR) -> $($env:VOL_SIZE_BYTES) bytes"
