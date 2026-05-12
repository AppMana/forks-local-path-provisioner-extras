# common.ps1 — shared helpers for the Windows local-path helper scripts.
#
# Sourced (. C:\opt\local-path-provisioner\common.ps1) by setup/teardown/resize.
#
# Env vars from the CSI controller: VOL_DIR VOL_SIZE_BYTES VOL_MODE VOL_QUOTA_TYPE
#
# NOTE: full FSRM enforcement is wired in M6. These helpers are the skeleton.

$ErrorActionPreference = 'Stop'

function Write-Log([string]$msg) {
    Write-Output ("{0} {1}" -f (Get-Date -Format o), $msg)
}

function Throw-Die([string]$msg) {
    Write-Log "ERROR: $msg"
    exit 1
}

# Resolve-FsType returns 'NTFS', 'ReFS', or the raw FileSystemType for the
# volume holding $Path.
function Resolve-FsType([string]$Path) {
    try {
        return (Get-Volume -FilePath $Path).FileSystemType
    } catch {
        return 'unknown'
    }
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
        'xfs'   { Throw-Die "quota type xfs requested on a Windows node" }
        'ext4'  { Throw-Die "quota type ext4 requested on a Windows node" }
        'btrfs' { Throw-Die "quota type btrfs requested on a Windows node" }
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
        default { Throw-Die "unknown VOL_QUOTA_TYPE: $requested" }
    }
}

# Ensure-FsrmAvailable verifies the FileServerResourceManager module is present
# (it ships with the FS-Resource-Manager Windows feature, installed in the
# servercore-based helper image).
function Ensure-FsrmAvailable {
    if (-not (Get-Module -ListAvailable -Name FileServerResourceManager)) {
        Throw-Die "FileServerResourceManager module not available; the helper image must include the FS-Resource-Manager feature"
    }
    Import-Module FileServerResourceManager -ErrorAction Stop
}
