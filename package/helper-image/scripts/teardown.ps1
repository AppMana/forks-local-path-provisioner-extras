# teardown.ps1 - remove the FSRM quota and the volume directory.
#
# env: VOL_DIR VOL_QUOTA_TYPE

. C:\opt\local-path-provisioner\common.ps1

if (-not $env:VOL_DIR) { Die "VOL_DIR not set" }

# Removing the quota is harmless even when there is none, so don't bother
# re-resolving the type - just attempt removal unconditionally.
Remove-FsrmHardQuota $env:VOL_DIR

if (Test-Path $env:VOL_DIR) {
    Remove-Item -Recurse -Force $env:VOL_DIR
}

Write-Log "teardown complete: $($env:VOL_DIR)"
