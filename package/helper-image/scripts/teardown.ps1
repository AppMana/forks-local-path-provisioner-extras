# teardown.ps1 — remove the FSRM quota and the volume directory.
#
# env: VOL_DIR VOL_QUOTA_TYPE

. C:\opt\local-path-provisioner\common.ps1

if (-not $env:VOL_DIR) { Throw-Die "VOL_DIR not set" }

# On teardown, an unsupported / unknown FS just means "no quota to remove".
$qtype = 'none'
switch (($env:VOL_QUOTA_TYPE)) {
    { $_ -in 'ntfs', 'refs' } { $qtype = $_ }
    'auto' {
        switch (Resolve-FsType $env:VOL_DIR) {
            'NTFS' { $qtype = 'ntfs' } 'ReFS' { $qtype = 'refs' } default { $qtype = 'none' }
        }
    }
    default { $qtype = 'none' }
}

if ($qtype -in 'ntfs', 'refs') {
    if (Get-Module -ListAvailable -Name FileServerResourceManager) {
        Import-Module FileServerResourceManager -ErrorAction SilentlyContinue
        Get-FsrmQuota -Path $env:VOL_DIR -ErrorAction SilentlyContinue | Remove-FsrmQuota -Confirm:$false -ErrorAction SilentlyContinue
    }
}

if (Test-Path $env:VOL_DIR) {
    Remove-Item -Recurse -Force $env:VOL_DIR
}

Write-Log "teardown complete: $($env:VOL_DIR)"
