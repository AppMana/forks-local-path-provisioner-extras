# escape=`
#
# local-path helper image (Windows). Built on windows-2022 by
# .github/workflows/helper-image.yml. servercore (not nanoserver) because the
# FileServerResourceManager PowerShell module — used for per-folder hard
# quotas on NTFS and ReFS — ships only with the FS-Resource-Manager Windows
# feature, which can't be installed on nanoserver.
ARG WINRELEASE=ltsc2022
FROM mcr.microsoft.com/windows/servercore:${WINRELEASE}

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference = 'Stop';"]

RUN Install-WindowsFeature -Name FS-FileServer, FS-Resource-Manager -IncludeManagementTools

COPY scripts\common.ps1 scripts\setup.ps1 scripts\teardown.ps1 scripts\resize.ps1 `
     scripts\snapshot.ps1 scripts\restore.ps1 `
     C:/opt/local-path-provisioner/

WORKDIR C:/opt/local-path-provisioner
