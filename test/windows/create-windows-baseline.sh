#!/usr/bin/env bash
# create-windows-baseline.sh — one-time bootstrap of disk-baseline-lpp.qcow2
# for the local-path-provisioner Windows e2e loop.
#
# Wraps the Calico harness in appmana-management/src/appmana_management/
# autoinstall/windows/ to install Windows Server 2022, run the ansible
# kubelet+containerd playbook, pre-pull the CSI images, and snapshot.
#
# See docs/windows-e2e.md for the bigger picture, including the one-time
# host network setup (br0 bridge, qemu bridge-helper, VyOS DHCP + BGP).
set -euo pipefail

APPMANA_MANAGEMENT=${APPMANA_MANAGEMENT:-$HOME/Documents/appmana/appmana-management/src/appmana_management}
WIN_AUTOINSTALL=${WIN_AUTOINSTALL:-$APPMANA_MANAGEMENT/autoinstall/windows}
WORK=${WORK:-/var/tmp/appmana-winauto-vm}
VM_IP=${VM_IP:-10.2.0.180}
DRIVER_IMAGE=${DRIVER_IMAGE:-ghcr.io/appmana/local-path-csi:latest}
HELPER_IMAGE=${HELPER_IMAGE:-ghcr.io/appmana/local-path-helper:latest}
REPO_ROOT=${REPO_ROOT:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)}

die() { echo "ERROR: $*" >&2; exit 1; }
note() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

[[ -d "$WIN_AUTOINSTALL" ]] || die "appmana-management/autoinstall/windows not found at $WIN_AUTOINSTALL"
[[ -x "$WIN_AUTOINSTALL/run-lab-test.sh" ]] || die "run-lab-test.sh not executable"
command -v ssh >/dev/null   || die "ssh missing"
command -v nc  >/dev/null   || die "nc missing"

# Step 1: full install + ansible playbook. Lands the VM at "kubelet
# registered+stopped, containerd 2.x ready".
note "Running run-lab-test.sh (this takes ~25-40 min the first time)"
bash "$WIN_AUTOINSTALL/run-lab-test.sh"

# Step 2: pre-pull the CSI images on the VM so per-iteration boots don't
# stall on image pulls.
note "Pre-pulling $DRIVER_IMAGE + $HELPER_IMAGE on the VM"
ssh -o StrictHostKeyChecking=no "administrator@$VM_IP" \
  "ctr -n k8s.io image pull $DRIVER_IMAGE && ctr -n k8s.io image pull $HELPER_IMAGE" \
  || die "image pre-pull failed (check that the VM has registry creds)"

# Step 3: stage deploy/csi/node-windows.yaml onto the VM for fast per-iteration apply.
note "Staging the Windows DaemonSet manifest at C:\\opt\\local-path-csi\\"
ssh "administrator@$VM_IP" 'mkdir C:\opt\local-path-csi -Force' || true
scp "$REPO_ROOT/deploy/csi/node-windows.yaml" "administrator@$VM_IP:C:/opt/local-path-csi/node-windows.yaml"

# Step 4: clean shutdown + snapshot.
note "Graceful shutdown + snapshot to disk-baseline-lpp.qcow2"
ssh "administrator@$VM_IP" 'shutdown /s /t 5' || true
sleep 30
[[ -f "$WORK/qemu.pid" ]] && kill "$(cat "$WORK/qemu.pid")" 2>/dev/null || true
sleep 5
[[ -f "$WORK/disk.qcow2" ]] || die "disk.qcow2 missing after shutdown — VM probably crashed"
cp -f "$WORK/disk.qcow2" "$WORK/disk-baseline-lpp.qcow2"
note "Baseline created at $WORK/disk-baseline-lpp.qcow2"
note "Per-iteration loop: bash test/windows/windows-e2e-iteration.sh"
