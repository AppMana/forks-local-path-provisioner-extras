# resize.ps1 - update the FSRM hard quota to a new size.
#
# Two modes (the CSI controller appends "-p <path> -s <size> -m <mode> -a <action>"):
#   -a resize       (default) set the quota for VOL_DIR to VOL_SIZE_BYTES
#   -a check-usage            print USAGE_BYTES=<n>
#
# env: VOL_DIR VOL_SIZE_BYTES VOL_QUOTA_TYPE

. C:\opt\local-path-provisioner\common.ps1

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

if (-not $env:VOL_DIR) { Die "VOL_DIR not set" }
$qtype = Resolve-QuotaType $env:VOL_DIR
switch ($qtype) {
    'none' { Write-Log "resize: no quota enforcement, metadata-only ($($env:VOL_DIR) -> $($env:VOL_SIZE_BYTES) bytes)" }
    { $_ -in 'ntfs', 'refs' } {
        Assert-FsrmAvailable
        if (-not (Get-FsrmQuota -Path $env:VOL_DIR -ErrorAction SilentlyContinue)) {
            Die "no FSRM quota at $($env:VOL_DIR); was this volume created with quota enforcement?"
        }
        $size = [int64]$env:VOL_SIZE_BYTES
        Write-Log "${qtype}: resize FSRM quota $($env:VOL_DIR) -> $size bytes"
        Set-FsrmHardQuota $env:VOL_DIR $size
    }
    default { Die "unhandled quota type: $qtype" }
}

Write-Log "resize complete: $($env:VOL_DIR) -> $($env:VOL_SIZE_BYTES) bytes"
