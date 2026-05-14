# Windows e2e via qemu VM (adapted from the Calico fork's loop)

The CSI driver's Linux path is covered by `kind`-based e2e in
`.github/workflows/csi-e2e.yml`. For Windows there is no equivalent kind
support — the Windows kubelet is an NT-kernel binary and can't run inside
a Linux container. `kubernetes-sigs/kind#1456` has tracked this since
2020; every credible workaround boils down to "spin up a separate
Windows-OS host that joins the Linux kind control plane." This doc walks
that path using the qemu/KVM Windows-Server-2022 harness in
`appmana-management/` (originally built for the Calico
`forks-calico-windows-ipv6` HNS L2Bridge work — see
`forks-calico-windows-ipv6/docs/qemu-test-loop.md`).

## Why kind can't do this natively

Kind nodes are Docker containers running a Linux kubelet. Linux containers
share the host kernel; a Windows kubelet binary needs NT syscalls that
Linux cannot provide. Docker Desktop on Windows can run Windows
containers, but only against the Windows host kernel — kind explicitly
provisions Linux nodes. Practical alternatives are all "Linux control
plane + Windows worker on a real Windows OS":

| Approach | What it is | Pros / cons |
|---|---|---|
| `kubernetes-sigs/sig-windows-dev-tools` | Vagrant + VirtualBox + Windows Server box, runs a kubeadm bootstrap and joins one Windows worker. | Most ergonomic local dev option. Heavier than kind (~6GB RAM for the VM), VirtualBox-only out of the box. |
| `kubernetes-sigs/sig-windows-tools` | PowerShell scripts that join a Windows host (real or VM) to an existing kubeadm cluster. | Bring-your-own Windows host. Useful if you already have one. |
| `kubernetes-sigs/windows-testing` + `cluster-api-provider-azure` | The upstream-CI rig — spins up real Azure Windows VMs. | Production-grade but cloud-only. |
| **This doc's harness** | The Calico fork's qemu/KVM lab VM + the `appmana-management` Ansible playbook (`playbook_kubernetes_containerd.yaml`) that installs containerd 2.2.1 + kubelet 1.34.2 unattended. | Fully local; same Windows build as production (`appmana-cluster-03` Windows nodes); ~5s per iteration after the one-time ~40min baseline. Requires the qemu/KVM host setup the Calico work already provisioned. |

The appmana harness was chosen because it already exists, the playbook
mirrors the production-cluster Windows-node bootstrap exactly, and the
iteration cadence beats Vagrant by a wide margin.

## TL;DR

```bash
# One-time, ~45 min:
bash test/windows/create-windows-baseline.sh       # boots qemu, installs Windows,
                                                   # runs the appmana playbook,
                                                   # snapshots disk-baseline-lpp.qcow2

# Per iteration, ~5 min:
bash test/windows/windows-e2e-iteration.sh         # restore baseline, deploy/upgrade
                                                   # the CSI driver, run the PVC test
                                                   # against the Windows node, report
```

## The harness reused

| Script (in `appmana-management/src/appmana_management/autoinstall/windows/`) | Purpose |
|---|---|
| `autounattend.xml` + `payload/post-install.ps1` | Unattended Windows Server 2022 install, enables WinRM + sshd, drops Administrator into auto-logon. |
| `vm-test.sh` | Boots qemu/KVM (q35 + KVM + OVMF UEFI), `52:54:00:01:23:45` MAC mapped to `10.2.0.180` on VyOS, an OOB slirp NIC with `127.0.0.1:16985→5985` for WinRM-over-NAT when the LAN side dies. `USE_BASELINE=1` restores a snapshot in ~5s. |
| `run-lab-test.sh` | Drives `playbook_kubernetes_containerd.yaml` end-to-end against the VM. Installs containerd 2.2.1 (cimfs snapshotter), kubelet 1.34.2, all the kube-proxy/Calico pieces, registers the kubelet service **stopped** so the L2Bridge / CSI plugins don't fire on the install boot. |
| `snapshot-baseline.sh` | Graceful-shutdown the VM + copy `disk.qcow2` → `disk-baseline.qcow2`. |
| `test-calico-iteration.sh` | The pattern: restore baseline → boot → `nssm start kubelet` → watch ARP/ping/winrm for N minutes → PASS/BRICK. The structural template for our `windows-e2e-iteration.sh`. |
| `appmana-management/.../inventory.yaml` | Contains the `qemu_lab_test` and `qemu_lab_test_oob` host entries the playbook targets. `skip_windows_updates: true` keeps baseline creation under 25 min; flip to `false` once for the first build to apply CUs past `20348.4773` (avoids the BSOD-on-first-L2Bridge issue documented in `forks-calico-windows-ipv6/docs/qemu-test-loop.md`). |

## Differences for local-path-provisioner

The Calico loop tested whether starting kubelet bricked the host's IPv4
management IP when HNS created the L2Bridge. Ours tests whether the CSI
driver can provision a PVC on the Windows node and a pod can mount it.
What changes:

- **Baseline contents** add the CSI driver's Windows DaemonSet
  (`deploy/csi/node-windows.yaml`) and the local-path-csi controller +
  helper images pre-pulled. The Calico baseline already has containerd +
  kubelet registered-stopped; we layer our DaemonSet on top before
  snapshotting.
- **Per-iteration check**: start kubelet → wait for the Windows node to be
  Ready → apply a PVC + Pod → assert the pod reaches Ready → exec a write
  + readback → delete → assert the on-disk directory is gone.
- **Network**: the VM lives on `br0` at `10.2.0.180`. It joins **whichever
  Linux control plane is reachable on that LAN**:
  - The production `appmana-cluster-03` API server. Don't use this for
    routine iteration — registering the CSI driver in production is risky
    and the VM's iterations would churn the production state.
  - A dedicated kind cluster on the host with
    `apiServerAddress: 10.2.0.<host-IP-on-br0>` + `apiServerPort: 6443`
    (or whatever). This is the recommended local-iteration setup. The
    kind config needs `networking.apiServerAddress` set to the host's br0
    IP so the VM can reach it.

## Recommended kind config for the host-side CP

```yaml
# test/windows/kind-cluster-windows-cp.yaml
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
networking:
  apiServerAddress: 10.2.0.42    # the host's br0 IP — the VM will dial this
  apiServerPort: 6443
nodes:
  - role: control-plane
    image: kindest/node:v1.35.1
```

After `kind create cluster --config=...`, generate a kubeconfig for the
Windows node:

```bash
kind get kubeconfig | sed "s|server: .*|server: https://10.2.0.42:6443|" \
  > /var/tmp/windows-node-kubeconfig.yaml
```

Drop that kubeconfig onto the Windows VM (the playbook already has a
`kubeconfig_yaml` ansible var to override). When kubelet starts, it joins
the host's kind cluster.

## Baseline creation (one-time)

`test/windows/create-windows-baseline.sh` is the wrapper that:

1. Calls `run-lab-test.sh` to do the install + containerd + kubelet
   playbook against a fresh disk.
2. SSH's in and pre-pulls the CSI driver + helper images:
   ```powershell
   ctr -n k8s.io image pull ghcr.io/appmana/local-path-csi:<sha>
   ctr -n k8s.io image pull ghcr.io/appmana/local-path-helper:<sha>
   ```
3. Drops the production-style `deploy/csi/node-windows.yaml` manifest at
   `C:\opt\local-path-csi\node-windows.yaml` so it can be applied by the
   per-iteration script without re-fetching.
4. Calls `snapshot-baseline.sh` to capture `disk-baseline-lpp.qcow2`.

## Per-iteration validation

`test/windows/windows-e2e-iteration.sh`:

1. `USE_BASELINE=1 vm-test.sh` — boot the snapshot, ~5s.
2. Wait for WinRM (≤ 5 min).
3. SSH in, `nssm start kubelet`.
4. From the host's kubeconfig, wait for the Windows node to be `Ready`
   (≤ 3 min).
5. `kubectl apply` the Windows DaemonSet (if not in the baseline) and
   `kubectl rollout status`.
6. Apply a small PVC + Pod where the pod has
   `nodeSelector: kubernetes.io/os: windows`.
7. Wait for the PVC to bind + the pod to reach Ready.
8. `kubectl exec` into the pod, write + read a file.
9. Delete the pod + PVC; verify the on-disk directory was cleaned up
   (via SSH into the VM: `Test-Path C:\opt\local-path-provisioner\<pv>`).
10. Report PASS / FAIL with a summary of any helper-pod errors captured
    from `kubectl -n local-path-storage logs`.

## What this does NOT cover

- **FSRM quota enforcement** — the qemu lab VM uses the same NTFS volume
  as Windows, so the FSRM hard-quota branch in `setup.ps1` runs but
  enforcement isn't stress-tested. To validate enforcement, add a
  fill-the-volume step that writes past `VOL_SIZE_BYTES` and expects an
  ERROR_DISK_FULL (32-bit `0x70`) or similar.
- **VSS shadow-copy snapshots** — VSS requires the Volume Shadow Copy
  service to be running on the volume. The baseline must enable it
  (`Set-Service VSS -StartupType Manual; Start-Service VSS`).
- **ReFS block-clone** — needs a ReFS-formatted disk. Add a second qcow2
  to `vm-test.sh` formatted as ReFS in the post-install step, and point
  the StorageClass at it.
- **HostProcess CSI plugin registration** — the Windows DaemonSet uses
  HostProcess containers (Kubernetes ≥ 1.23). The harness validates the
  full kubelet↔plugin path; if the plugin's UDS at
  `C:\var\lib\kubelet\plugins\local-path.appmana.com\csi.sock` doesn't
  register, the node logs will show
  `node-driver-registrar` errors visible in `kubectl logs`.

## Status

The harness scripts in `test/windows/` are best-effort scaffolding —
they parallel the Calico loop in structure but have not been executed
end-to-end yet (creating the baseline requires the qemu host + the
Windows ISO + the `appmana-management` Ansible playbook to be wired up;
see `forks-calico-windows-ipv6/docs/qemu-test-loop.md` §"Network
requirements" for the one-time host setup). The Linux kind e2e in
`.github/workflows/csi-e2e.yml` runs on every PR; the Windows VM e2e is
opt-in (`workflow_dispatch` only, with `runs-on: [self-hosted, qemu]`).
