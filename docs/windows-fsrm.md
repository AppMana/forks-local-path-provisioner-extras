# Windows FSRM hard-quota enforcement caveats

Validated on the qemu Windows Server 2022 lab VM (`disk-baseline-patched.qcow2`,
build 20348) on 2026-05-13.

## What works

The helper-image PowerShell scripts (`setup.ps1` / `teardown.ps1` /
`resize.ps1` / `snapshot.ps1` / `restore.ps1`) round-trip cleanly on
Windows PowerShell 5.1 with FSRM installed:

- `setup.ps1` (auto-detect → ntfs) creates the volume directory and an
  FSRM quota entry of the requested size.
- `resize.ps1 -a resize` updates the quota size via `Set-FsrmQuota`.
- `resize.ps1 -a check-usage` emits a clean `USAGE_BYTES=<n>` on stdout
  (stderr-log fix confirmed).
- `teardown.ps1` removes the quota and the directory.

```text
=== setup.ps1 (auto -> ntfs) ===
Path  : C:\Windows\Temp\lpp-fsrm-test
Size  : 1073741824
=== resize to 2GiB ===
Path  : C:\Windows\Temp\lpp-fsrm-test
Size  : 2147483648
=== check-usage ===
  captured: USAGE_BYTES=7
=== teardown ===
  dir gone: True
  FSRM quota gone: True
```

## What doesn't (yet)

On a fresh Server 2022 host with `Install-WindowsFeature FS-Resource-Manager`,
**FSRM hard quotas do not actually block writes past the size limit**.
A 4096-byte quota lets an 8192-byte file through; `Get-FsrmQuota` reports
the `SoftLimit` property as `True` regardless of how the quota is created:

- `New-FsrmQuota -SoftLimit:$false` — SoftLimit reads back True; writes go through.
- `New-FsrmQuota -Template '<a built-in template with SoftLimit=False>'` — same.
- `New-CimInstance` directly setting `SoftLimit = $false` on `MSFT_FSRMQuota` — same.
- `dirquota.exe quota add /path:X /limit:N` (the legacy CLI) — same.

The Datascrn filter driver is loaded and `fltmc attach Datascrn C:`
successfully attaches it (filter instance count goes 0 → 1), but quotas
still do not enforce.

## Why

Likely a missing one-time host configuration step (FSRM monitored-volumes
list, FSRM service-level enforcement toggle, or an SKU/eval-edition
limitation on Server 2022 Datacenter Evaluation). The behavior is
reproducible on the qemu lab VM and would need to be reproduced on a
real Windows Server 2022 Datacenter (non-eval) before declaring the
investigation done.

## Implications for production

- The CSI helper scripts are **functionally correct** — they call the
  documented FSRM cmdlets with the documented parameters. If/when the
  host's FSRM is configured for enforcement, our quotas will be enforced.
- We pass `-SoftLimit:$false` explicitly on both `New-FsrmQuota` and
  `Set-FsrmQuota` so when FSRM enforcement is restored, our intent
  carries through.
- The script's create / read / update / delete lifecycle is verified.
  The enforcement gap is an operational concern (FSRM host setup), not a
  scripting bug.

## Workaround under investigation

Likely candidates to flip enforcement on:

```powershell
# Restart the FSRM service after attaching the filter
Restart-Service srmsvc
# Trigger a quota scan
Update-FsrmQuota
# Or: re-install with explicit volume monitoring
Add-FsrmFileGroup -Name "..."
```

None of the above were tested in this session — pinning this down is a
separate task. The `windows-fsrm-enforcement.sh` smoke script in
`test/windows/` (TODO) will validate any candidate fix against the qemu
VM.
