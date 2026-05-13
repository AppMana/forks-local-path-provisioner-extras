# Windows FSRM hard-quota enforcement — paths matter

Validated on the qemu Windows Server 2022 lab VM (`disk-baseline-patched.qcow2`,
build 20348) on 2026-05-13.

## What works

Both the helper-image PowerShell scripts AND raw FSRM cmdlets enforce
hard quotas correctly **when the quota is set on a non-system path**:

```text
  C:\data\quota-test                              SoftLimit=False -> HARD
  C:\opt\local-path-provisioner\quota-test        SoftLimit=False -> HARD
  C:\Windows\Temp\quota-test                      SoftLimit=True  -> SOFT (wrote 8192)
```

A 4 KiB hard quota on `C:\data\…` or `C:\opt\local-path-provisioner\…`
rejects an 8 KiB write with `ERROR_DISK_FULL` ("There is not enough space
on the disk."). The same write into `C:\Windows\Temp\…` goes through —
FSRM excludes Windows-system paths from enforcement and silently flips
the quota to soft.

## What we ship

`common.ps1`'s `Set-FsrmHardQuota` explicitly passes `-SoftLimit:$false`
to both `New-FsrmQuota` and `Set-FsrmQuota`. With the default StorageClass
`nodePath` of `/opt/local-path-provisioner` → `C:\opt\local-path-provisioner`
on Windows nodes, that lands in the FSRM-enforced region of the FS.

## What the operator must know

- **Do not configure a Windows `nodePath` under `C:\Windows\…`,
  `C:\Program Files\…`, or other system-protected directories.** FSRM
  silently drops enforcement there.
- A vanilla `C:\data` or `C:\opt\local-path-provisioner` works.
- The FS-Resource-Manager Windows feature must be installed on the node
  (`Install-WindowsFeature FS-Resource-Manager -IncludeManagementTools`).
  Our `windows.Dockerfile` installs it inside the helper-image servercore
  base; that's enough for the helper-pod-side scripts. The host kubelet
  doesn't need it.
- On a fresh FSRM install, `Restart-Service srmsvc` once after the
  feature install picks up the filter; usually a reboot does this
  implicitly.

## Verification commands

Drop a test quota and try to overflow it:

```powershell
New-Item -ItemType Directory -Force C:\opt\local-path-provisioner\quota-test | Out-Null
New-FsrmQuota -Path C:\opt\local-path-provisioner\quota-test -Size 4096 -SoftLimit:$false
[System.IO.File]::WriteAllBytes("C:\opt\local-path-provisioner\quota-test\big.bin", (New-Object byte[] 8192))
# Expected: Exception "There is not enough space on the disk."
Get-FsrmQuota -Path C:\opt\local-path-provisioner\quota-test | Format-List Path, Size, SoftLimit
# Expected: SoftLimit = False
```

If `SoftLimit` reads `True` despite passing `-SoftLimit:$false`, the most
likely cause is that the path falls under a system-protected directory.
Move the quota to `C:\data\…` and retry.
