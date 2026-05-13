# common.ps1 - shared helpers for the Windows local-path helper scripts.
#
# Sourced (. C:\opt\local-path-csi-scripts\common.ps1) by setup/teardown/
# resize/snapshot/restore.
#
# Env vars from the CSI controller: VOL_DIR VOL_SIZE_BYTES VOL_MODE
# VOL_QUOTA_TYPE [SNAP_DIR]

$ErrorActionPreference = 'Stop'

# Write-Log writes a timestamped line to STDERR. It must not go to stdout
# because PowerShell folds all pipeline output into a function's return value
# (Resolve-QuotaType etc. would be corrupted by a stray log line). The kubelet
# merges the helper pod's stdout+stderr into the container log, so the
# controller still sees these lines.
function Write-Log([string]$msg) {
    $line = ((Get-Date -Format o).ToString()) + ' ' + $msg
    [Console]::Error.WriteLine($line)
}

# Die logs an error and exits non-zero. Not named Verb-Noun on purpose so
# PSScriptAnalyzer's PSUseApprovedVerbs rule leaves it alone.
function Die([string]$msg) {
    Write-Log "ERROR: $msg"
    exit 1
}

# Resolve-FsType returns 'NTFS', 'ReFS', or the raw FileSystemType for the
# volume holding $Path.
function Resolve-FsType([string]$Path) {
    try { return (Get-Volume -FilePath $Path).FileSystemType }
    catch { return 'unknown' }
}

# Resolve-VolumeDriveLetter returns the drive letter (e.g. 'D') for the volume
# holding $Path, or '' if it can't be determined.
function Resolve-VolumeDriveLetter([string]$Path) {
    try {
        $dl = (Get-Volume -FilePath $Path).DriveLetter
        if ($dl) { return [string]$dl }
        return ''
    } catch { return '' }
}

# Resolve-QuotaType maps $env:VOL_QUOTA_TYPE to a concrete type, auto-detecting
# from the filesystem when 'auto'. Returns one of: ntfs refs none.
function Resolve-QuotaType([string]$Path) {
    $requested = $env:VOL_QUOTA_TYPE
    if (-not $requested) { return 'none' }
    switch ($requested.ToLower()) {
        'none'  { return 'none' }
        ''      { return 'none' }
        'ntfs'  { return 'ntfs' }
        'refs'  { return 'refs' }
        'xfs'   { Die "quota type xfs requested on a Windows node" }
        'ext4'  { Die "quota type ext4 requested on a Windows node" }
        'btrfs' { Die "quota type btrfs requested on a Windows node" }
        'auto'  {
            $fs = Resolve-FsType $Path
            switch ($fs) {
                'NTFS' { return 'ntfs' }
                'ReFS' { return 'refs' }
                default {
                    Write-Log "auto-detect: filesystem $fs has no FSRM quota support, skipping enforcement"
                    return 'none'
                }
            }
        }
        default { Die "unknown VOL_QUOTA_TYPE: $requested" }
    }
}

# Assert-FsrmAvailable verifies the FileServerResourceManager module is present
# (it ships with the FS-Resource-Manager Windows feature, installed in the
# servercore-based helper image) and imports it.
function Assert-FsrmAvailable {
    if (-not (Get-Module -ListAvailable -Name FileServerResourceManager)) {
        Die "FileServerResourceManager module not available; the helper image must include the FS-Resource-Manager feature"
    }
    Import-Module FileServerResourceManager -ErrorAction Stop
}

# Set-FsrmHardQuota creates or updates an FSRM HARD byte quota on $Path.
# Hard quotas reject writes that would exceed the limit. The Windows Server
# 2022 FSRM cmdlets default to soft limits despite the docs saying
# otherwise; explicitly pass -SoftLimit:$false on both create and update.
function Set-FsrmHardQuota([string]$Path, [int64]$SizeBytes) {
    Assert-FsrmAvailable
    try {
        $existing = Get-FsrmQuota -Path $Path -ErrorAction SilentlyContinue
        if ($existing) {
            Set-FsrmQuota -Path $Path -Size $SizeBytes -SoftLimit:$false -ErrorAction Stop | Out-Null
        } else {
            New-FsrmQuota -Path $Path -Size $SizeBytes -SoftLimit:$false -Description "local-path-provisioner" -ErrorAction Stop | Out-Null
        }
        Get-FsrmQuota -Path $Path | Format-List Path, Size, Usage, SoftLimit
    } catch {
        Die "FSRM quota operation failed for $Path : $($_.Exception.Message)"
    }
}

# Remove-FsrmHardQuota removes any FSRM quota at $Path. Best-effort.
function Remove-FsrmHardQuota([string]$Path) {
    if (-not (Get-Module -ListAvailable -Name FileServerResourceManager)) { return }
    Import-Module FileServerResourceManager -ErrorAction SilentlyContinue
    Get-FsrmQuota -Path $Path -ErrorAction SilentlyContinue | Remove-FsrmQuota -Confirm:$false -ErrorAction SilentlyContinue
}
