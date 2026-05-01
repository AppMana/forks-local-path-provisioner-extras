# forks-local-path-provisioner-extras — implementation plan

Fork: `appmana/forks-local-path-provisioner-extras` (forked from `rancher/local-path-provisioner` at master).

## 1. Scope

Extend local-path-provisioner with:

1. **CSI driver** wrapping the existing helper-pod core, so we can ship the standard storage APIs:
   - `CSIStorageCapacity` for capacity-aware scheduling.
   - `VolumeSnapshot` / `VolumeSnapshotContent` for snapshots.
   - `ControllerExpandVolume` (and `NodeExpandVolume`) for resize.
2. **Filesystem-aware quota + resize**: ext4, xfs, btrfs (Linux); NTFS, ReFS (Windows).
3. **Snapshots** per FS: btrfs (subvolume snapshot), xfs (reflink-clone), NTFS/ReFS (FSRM/VSS/block-clone). State recorded in node-local sidecar JSON files alongside the volume directory.
4. **Multi-platform helper image** built via the existing `appmana-containers-automated/<image>` + Tekton `build-automation` chart pipeline (the same pattern that builds `appmana-shared/busybox`). New container entry: `appmana-containers-automated/local-path-helper/`.

## 2. Why CSI

`CSIStorageCapacity` is a CSI-only API. The kube-scheduler's `VolumeBinding` plugin consults `CSIStorageCapacity` objects only when the PVC's StorageClass points at a CSI provisioner with `CSIDriver.spec.storageCapacity: true`. There is no in-tree-style equivalent. Same constraint applies to `VolumeSnapshot`: the snapshot controller rejects non-CSI PVs (issues #431, #433 upstream confirm this).

The existing `rancher/local-path-provisioner` is built on `sigs.k8s.io/sig-storage-lib-external-provisioner`, not the CSI gRPC interface. To get `CSIStorageCapacity` and `VolumeSnapshot`, the fork has to be a CSI driver.

## 3. Prior art (upstream rancher/local-path-provisioner)

| ref | state | relevance |
|---|---|---|
| PR #558 (ours, OPEN) | open | XFS-only quota + per-path capacity budget + annotation-driven resize. **We keep its helper scripts and CapacityTracker but discard the resize_controller.go** (replaced by CSI ControllerExpandVolume). |
| PR #559 (MERGED) | merged | StorageClass `nodeAffinityKey` parameter pattern. |
| PR #350 (CLOSED) | closed | Earlier resize attempt using `longhorn/external-resizer`. Not applicable for CSI. |
| PR #24 (2019, CLOSED) | closed | Abandoned multi-arch attempt. No upstream multi-platform CI. |
| Issue #21 (OPEN since 2020) | open | Btrfs subvolume request. |
| Issue #81 (OPEN since 2020, 27 comments) | open | **Most-requested feature**: VolumeSnapshot support. CSI is the only path. |
| Issue #285 (OPEN) | open | Capacity-aware scheduling, references TopoLVM (CSI driver using `CSIStorageCapacity`). |
| Issue #442 (CLOSED) | closed | Windows node support. **Already solved here** via `appmana-shared/busybox` multi-platform image — issue #442 is moot for this fork. |
| Issue #67 (CLOSED) | closed | Per-node `available_cmd` hook idea. Replaced by CSI `GetCapacity`. |
| Issue #509 (CLOSED) | closed | Confirms `WaitForFirstConsumer` + delayed binding is correct. |

## 4. Architecture

### 4.1 CSI driver shape

A single binary, registered as one CSIDriver. Container layout follows the standard kubernetes-csi pattern:

- **Controller pod** (`Deployment`, replica 1, leader-elected later): runs
  - our binary in `--mode=controller`,
  - `csi-provisioner` sidecar (handles `CreateVolume`/`DeleteVolume` from PVCs),
  - `csi-resizer` sidecar (handles `ControllerExpandVolume` from PVC capacity changes),
  - `csi-snapshotter` sidecar (handles `VolumeSnapshot`/`VolumeSnapshotContent`),
  - `csi-attacher` sidecar (no-op for local volumes but required by the spec for `Local` PVs).
- **Node pod** (`DaemonSet`): runs
  - our binary in `--mode=node`,
  - `node-driver-registrar` sidecar.
  - publishes `CSIStorageCapacity` per `(node, storageClass, topologySegment)` — the registrar's storage capacity tracking is enabled via `--enable-capacity` and the `external-provisioner --feature-gates=Topology=true --enable-capacity --capacity-ownerref-level=2` flags on the controller side. (Both controller-side and node-side are valid; we publish from the controller using the `external-provisioner`'s built-in capacity reporter, which calls our `GetCapacity` RPC.)

```
                   ┌─ csi-provisioner ──► CreateVolume / DeleteVolume RPCs
                   │
                   ├─ csi-resizer    ──► ControllerExpandVolume RPC
   controller pod ─┼─ csi-snapshotter──► CreateSnapshot / DeleteSnapshot RPCs
                   ├─ csi-attacher  (no-op for local; required for Local PVs)
                   │
                   └─ our binary (--mode=controller)
                        ├─ implements ControllerServer (gRPC over UDS)
                        ├─ implements IdentityServer
                        └─ runs the existing helper-pod machinery from provisioner.go
                            (configmap config, helper pod dispatch, log capture)

   node pod (DS) ──► our binary (--mode=node) ──► NodeServer
                     ├─ NodeStageVolume / NodePublishVolume = bind-mount volume dir
                     ├─ NodeExpandVolume = run helper-pod resize action
                     ├─ NodeGetVolumeStats = statvfs() on volume dir
                     └─ NodeGetCapabilities = STAGE_UNSTAGE | EXPAND_VOLUME | GET_VOLUME_STATS

                     node-driver-registrar sidecar ──► kubelet plugin registration
```

### 4.2 Reuse vs replace from upstream

| upstream piece | fate |
|---|---|
| `main.go` urfave/cli flag parsing, configmap loading, kubeconfig | reused; add `--mode=controller|node` switch |
| `provisioner.go` `Provision()`/`Delete()` (sigs.k8s.io/sig-storage-lib) | **replaced by ControllerServer.CreateVolume/DeleteVolume**, but the *body* of these functions (helper pod dispatch, path selection, `pickConfig`, `getPathOnNode`, `pathFromPattern`) is reused verbatim |
| `provisioner.go` CapacityTracker (from PR #558) | **kept**; rewired to feed `GetCapacity` RPC |
| `provisioner.go` `createHelperPod` | **kept**; called from CSI RPCs |
| `resize_controller.go` (from PR #558) | **replaced by csi-resizer + ControllerExpandVolume + NodeExpandVolume**; the resize *script* in the helper image is kept |
| helper scripts (`setup`, `teardown`, `resize` from PR #558) | **kept and extended** for ext4/btrfs/Windows/snapshot |
| ConfigMap `config.json` | **kept** as the source of truth for nodePathMap, minSize/maxSize, maxCapacity |
| Helm chart `deploy/chart/local-path-provisioner` | reworked: rename to `csi-local-path`, add CSI sidecars, DaemonSet, CSIDriver, VolumeSnapshotClass |
| `examples/` from PR #558 | kept |
| `test/` quota_*_test.go from PR #558 | kept; add CSI-specific e2e |

### 4.3 Branch onto PR #558, then layer CSI

```
upstream/master
  └── doctorpangloss:pr-558-xfs-quota-capacity-resize  (PR #558, our open PR)
        └── appmana/extras-base                         (rebased + minor cleanups)
              ├── extras-csi          (CSI driver wrapper, removes the in-tree provisioning controller)
              ├── extras-fs-multi     (ext4, btrfs, NTFS, ReFS branches in helper scripts)
              ├── extras-snapshot     (VolumeSnapshot + per-FS snapshot primitives)
              ├── extras-helper-image (new appmana-containers-automated entry)
              └── extras-capacity     (GetCapacity wiring + CSIStorageCapacity publishing)
```

`extras-csi` is the largest change and lands first; the rest layer on top.

## 5. Per-feature design

### 5.1 CSI ControllerServer.CreateVolume → existing helper-pod path

```go
func (cs *ControllerServer) CreateVolume(ctx, req) (*CreateVolumeResponse, error) {
    // PVC capacity, name, parameters live in req.Parameters / req.CapacityRange
    // SelectedNode comes from the AccessibilityRequirements.Preferred[0].Segments
    selectedNode := pickNode(req.AccessibilityRequirements)
    cfg, _ := cs.provisioner.pickConfig(req.Parameters["storageClassName"])
    basePath, _ := cs.provisioner.getPathOnNode(selectedNode, req.Parameters["nodePath"], cfg)
    folderName := computeFolderName(req)  // existing pathPattern / safe-default logic
    path := filepath.Join(basePath, folderName)

    if !cs.provisioner.capacityTracker.TryAllocate(selectedNode, basePath, sizeBytes, maxCap) {
        return nil, status.Error(codes.ResourceExhausted, "path budget exceeded")
    }
    if err := cs.provisioner.createHelperPod(ActionTypeCreate, ..., volumeOptions{...}); err != nil {
        cs.provisioner.capacityTracker.Release(selectedNode, basePath, sizeBytes)
        return nil, err
    }

    return &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            VolumeId:      pvName,
            CapacityBytes: sizeBytes,
            VolumeContext: map[string]string{"path": path, "node": selectedNode, "fsType": detectedFS},
            AccessibleTopology: []*csi.Topology{{
                Segments: map[string]string{"kubernetes.io/hostname": selectedNode},
            }},
        },
    }, nil
}
```

The CSIDriver registers `kubernetes.io/hostname` as the topology key; combined with `volumeBindingMode: WaitForFirstConsumer` on the StorageClass, kube-scheduler picks the node *before* CreateVolume and passes it via `AccessibilityRequirements`. Identical control flow to today's `SelectedNode` annotation.

### 5.2 CSIStorageCapacity → ControllerServer.GetCapacity

The csi-provisioner sidecar (with `--enable-capacity --capacity-ownerref-level=2`) periodically polls our `GetCapacity` RPC for each `(storageClass, topology)` tuple. We answer per-node free bytes:

```go
func (cs *ControllerServer) GetCapacity(ctx, req) (*GetCapacityResponse, error) {
    node := req.AccessibleTopology.Segments["kubernetes.io/hostname"]
    cfg, _ := cs.provisioner.pickConfig(req.Parameters["storageClassName"])
    paths := cfg.NodePathMap[node].Paths
    // run a NodeGetCapacity-style helper pod (or read a cached value the node-side reporter
    // pushes to a NodeAnnotation): returns sum of statvfs(path).f_bavail across configured paths,
    // minus the path's outstanding maxCapacity allocation, never negative.
    free := cs.capacityReporter.FreeBytesForNode(node, paths)
    return &csi.GetCapacityResponse{
        AvailableCapacity: free,
        MaximumVolumeSize: wrapperspb.Int64(largestPathFreeBytes(node, paths)),
    }, nil
}
```

**Where free bytes come from**: the node-mode binary runs a small loop (every 30s) that statvfs's each configured path and writes the result to a Node annotation `local.path.provisioner/free-bytes-by-path: '{"/opt/lpp": 274877906944}'`. The controller reads from these annotations rather than dispatching helper pods on every poll. Result: zero per-poll pod churn.

`CSIStorageCapacity` objects are owned by the controller `Deployment`; `--capacity-ownerref-level=2` ties their lifecycle to the parent (so they're GC'd cleanly on driver uninstall).

### 5.3 Resize → ControllerExpandVolume + NodeExpandVolume

- **ControllerExpandVolume**: validates the new size against `minSize`/`maxSize` from the StorageClass; updates the CapacityTracker; returns `NodeExpansionRequired: true`.
- **NodeExpandVolume**: runs the helper pod with `ActionTypeResize`, which dispatches to the `resize-linux.sh` or `resize-windows.ps1` script. The script switches on FS type and calls the right tool (`xfs_quota limit`, `setquota -P`, `btrfs qgroup limit`, `Set-FsrmQuota`).
- **Shrink**: CSI doesn't support shrinking via the standard PVC capacity field. Either:
  1. Keep PR #558's annotation-driven shrink (controller watches `local.path.provisioner/requested-size` PVC annotation and runs a helper pod), OR
  2. Drop shrink for v1 (it's not a standard CSI capability anyway).

Decision: **drop shrink for v1**; document that users wanting to reclaim space delete and recreate the PVC. PR #558's resize_controller.go shrink path is removed.

### 5.4 Filesystem-aware quotas

Per-FS branches in `setup`/`teardown`/`resize`/`snapshot` shell scripts. Each script auto-detects FS type at the volume's parent dir:

```sh
fsType=$(stat -f -c '%T' "$parentDir")
case "$fsType" in
  xfs)            quotaType="xfs"   ;;
  ext2/ext3)      quotaType="ext4"  ;;   # stat -f reports ext4 as ext2/ext3 on most kernels
  btrfs)          quotaType="btrfs" ;;
  *) [ "$VOL_QUOTA_TYPE" = "auto" ] && fail "unsupported FS: $fsType" ;;
esac
```

| FS | mechanism | mount option | tool | resize | teardown |
|---|---|---|---|---|---|
| xfs | project quota | `prjquota`/`pquota` | `xfs_quota -x -c 'limit -p bhard=N pvc'` | rerun limit | `limit -p bhard=0`, scrub /etc/projects, /etc/projid |
| ext4 | project quota | `prjquota` (mkfs.ext4 `-O quota,project`) | `chattr +P -p <id>` + `setquota -P <id> 0 N 0 0 <mp>` | rerun setquota | `setquota -P <id> 0 0 0 0`, scrub project files |
| btrfs | qgroup | none (`btrfs quota enable <mp>` once) | `btrfs subvolume create $VOL_DIR` + `btrfs qgroup limit N $VOL_DIR` | rerun qgroup limit | `btrfs subvolume delete $VOL_DIR` |
| ntfs | FSRM per-folder | n/a | `New-FsrmQuota -Path X -Size N` | `Set-FsrmQuota -Path X -Size N` | `Remove-FsrmQuota -Path X` |
| refs | FSRM per-folder (Windows Server 2019+) | n/a | same as NTFS | same | same |

**Btrfs subvolume vs directory**: PR #558's `setup` does `mkdir -p`. For btrfs we must use `btrfs subvolume create` instead — detected before any mkdir.

**ext4 project ID pool**: shared with xfs (same `/etc/projid` + `/etc/projects` files; PR #558 already mounts these as hostPaths). ID allocation lock (`/var/lock/local-path-quota.lock`, already in PR #558) is shared across FSes.

### 5.5 Snapshots → standard VolumeSnapshot CRDs

Implements `csi.ControllerServer.CreateSnapshot`/`DeleteSnapshot`/`ListSnapshots`. The csi-snapshotter sidecar wires up `VolumeSnapshot` → `VolumeSnapshotContent` → our RPC.

```go
func (cs *ControllerServer) CreateSnapshot(ctx, req) (*CreateSnapshotResponse, error) {
    src := lookupVolume(req.SourceVolumeId)         // node, path, fsType
    snapName := req.Name                            // VolumeSnapshotContent.Spec.VolumeSnapshotRef.Name
    snapPath := filepath.Join(filepath.Dir(src.Path), ".snapshots", snapName)

    if err := cs.provisioner.createHelperPod(ActionTypeSnapshot, ..., volumeOptions{
        Source: src.Path, Target: snapPath, FSType: src.FSType,
    }); err != nil { return nil, err }

    // The helper script wrote a sidecar JSON file; capture it via the existing
    // saveHelperPodLogs() machinery — the script echoes the JSON to stdout.
    return &csi.CreateSnapshotResponse{
        Snapshot: &csi.Snapshot{
            SnapshotId:    snapID,
            SourceVolumeId: src.VolumeID,
            CreationTime:  timestamppb.New(time.Now()),
            ReadyToUse:    true,
            SizeBytes:     usageAtSnapshotTime,
        },
    }, nil
}
```

**Per-FS snapshot primitive**:

| FS | command | atomicity | space | restore |
|---|---|---|---|---|
| btrfs | `btrfs subvolume snapshot -r $src $dst` | atomic, point-in-time | CoW (≈0 at create) | `btrfs subvolume snapshot $dst $newvol` |
| xfs | `cp --reflink=always -aR $src $dst` (requires xfs reflink, mkfs.xfs `-m reflink=1`, default since xfsprogs 5.1) | **NOT atomic — workload must be quiesced** | per-file CoW | `cp --reflink=always -aR $dst $newvol` |
| ext4 | unsupported — reject `CreateSnapshot` with `codes.FailedPrecondition` | — | — | — |
| ntfs (servercore) | volume-level VSS: `vssadmin create shadow /for=D:` then `robocopy` only the PVC's dir out into snapshots area | volume-atomic | redirect-on-write | `robocopy` from snapshot dir back |
| refs | `Copy-Item -Path $src -Destination $dst` (auto-uses block clone if same volume + ReFS ≥3.1) | per-file | per-file CoW | `Copy-Item` back |

**Node-local sidecar JSON** (per user request: store all state on the node):

```
<basePath>/.snapshots/<snap-name>.json
{
  "snapshotId": "snap-7e3c",
  "sourceVolumeId": "pvc-abc123",
  "sourcePath": "/opt/lpp/pvc-abc123_default_data",
  "snapshotPath": "/opt/lpp/.snapshots/snap-7e3c",
  "fsType": "btrfs",
  "node": "node-12",
  "sizeBytes": 1073741824,
  "creationTime": "2026-05-01T15:32:01Z"
}
```

The CSI driver reconstructs `SnapshotContent` state from these files on restart by walking each configured `nodePath/.snapshots/`. The K8s `VolumeSnapshotContent` CR is the cluster-side mirror; the node-local JSON is the source of truth and survives controller-pod loss.

**Restore**: `CreateVolume` with `VolumeContentSource.Snapshot.SnapshotId` set → helper pod with `ActionTypeRestoreSnapshot` → btrfs `subvolume snapshot`, xfs `cp --reflink=always`, etc. Bound to the same node (snapshots are node-local; cross-node restore is out of scope for v1).

**Documented limitations**:
- ext4: snapshots unsupported.
- xfs: requires reflink-enabled FS; not atomic across in-flight writes — applications must quiesce.
- All FSes: snapshot is on the same node as the source; cross-node clone requires application-level replication.

### 5.6 Helper image

Built in this fork via GitHub Actions. Layout:

```
package/helper-image/
  linux.Dockerfile
  windows.Dockerfile
  scripts/
    common.sh
    setup-linux.sh, teardown-linux.sh, resize-linux.sh, snapshot-linux.sh, restore-linux.sh
    setup-windows.ps1, teardown-windows.ps1, resize-windows.ps1, snapshot-windows.ps1, restore-windows.ps1
```

**linux.Dockerfile** (multi-arch via buildx, `linux/amd64,linux/arm64`):

```dockerfile
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    xfsprogs btrfs-progs e2fsprogs quota \
    util-linux coreutils \
    && rm -rf /var/lib/apt/lists/*
COPY scripts/common.sh scripts/*-linux.sh /opt/local-path-provisioner/
RUN chmod +x /opt/local-path-provisioner/*
```

**windows.Dockerfile** (servercore, ltsc2022):

```dockerfile
# escape=`
ARG WINRELEASE=ltsc2022
FROM mcr.microsoft.com/windows/servercore:${WINRELEASE}
USER ContainerAdministrator
SHELL ["powershell", "-NoProfile", "-Command"]
RUN Install-WindowsFeature FS-FileServer, FS-Resource-Manager -IncludeManagementTools
COPY scripts\common.sh scripts\*-windows.ps1 C:/opt/local-path-provisioner/
WORKDIR C:/opt/local-path-provisioner
```

**Workflow** `.github/workflows/helper-image.yml`:

```yaml
name: helper-image
on:
  push:
    branches: [appmana-extras]
    paths: ['package/helper-image/**']
  workflow_dispatch:

env:
  REGISTRY: ghcr.io
  IMAGE: ${{ github.repository_owner }}/local-path-helper

jobs:
  linux:
    runs-on: ubuntu-24.04
    permissions: { contents: read, packages: write }
    outputs:
      digest: ${{ steps.build.outputs.digest }}
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-qemu-action@v3
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: build
        uses: docker/build-push-action@v6
        with:
          context: package/helper-image
          file: package/helper-image/linux.Dockerfile
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ env.REGISTRY }}/${{ env.IMAGE }}:linux-${{ github.sha }}

  windows:
    runs-on: windows-2022
    permissions: { contents: read, packages: write }
    steps:
      - uses: actions/checkout@v4
      - run: docker login ${{ env.REGISTRY }} -u ${{ github.actor }} -p ${{ secrets.GITHUB_TOKEN }}
      - run: |
          docker build -t ${{ env.REGISTRY }}/${{ env.IMAGE }}:windows-${{ github.sha }} `
            -f package/helper-image/windows.Dockerfile `
            --build-arg WINRELEASE=ltsc2022 `
            package/helper-image
          docker push ${{ env.REGISTRY }}/${{ env.IMAGE }}:windows-${{ github.sha }}

  manifest:
    needs: [linux, windows]
    runs-on: ubuntu-24.04
    permissions: { contents: read, packages: write }
    steps:
      - uses: imjasonh/setup-crane@v0.4
      - run: |
          crane auth login ${{ env.REGISTRY }} -u ${{ github.actor }} -p ${{ secrets.GITHUB_TOKEN }}
          crane index append \
            -m ${{ env.REGISTRY }}/${{ env.IMAGE }}:linux-${{ github.sha }} \
            -m ${{ env.REGISTRY }}/${{ env.IMAGE }}:windows-${{ github.sha }} \
            -t ${{ env.REGISTRY }}/${{ env.IMAGE }}:${{ github.sha }} \
            -t ${{ env.REGISTRY }}/${{ env.IMAGE }}:latest
```

`crane index append` follows the same combine pattern as the cluster's Tekton pipeline (`appmana-cluster/src/charts/build-automation/templates/pipeline.yaml:899`).

### 5.7 Capacity-aware scheduling — entirely via CSI

No scheduler extender. Wiring:

```yaml
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata: { name: local-path.appmana.io }
spec:
  attachRequired: false
  podInfoOnMount: true
  storageCapacity: true                    # ← unlocks CSIStorageCapacity scheduling
  volumeLifecycleModes: [Persistent]

apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: { name: local-path-csi }
provisioner: local-path.appmana.io
volumeBindingMode: WaitForFirstConsumer    # ← required for topology-aware scheduling
allowVolumeExpansion: true
parameters:
  nodePath: /opt/local-path-provisioner
  quotaType: auto                          # xfs|ext4|btrfs|ntfs|refs|none|auto
  minSize: 1Gi
  maxSize: 50Gi
```

Kube-scheduler's `VolumeBinding` plugin filters out nodes with insufficient capacity by reading `CSIStorageCapacity` objects (which our csi-provisioner sidecar publishes per topology segment). No custom scheduler config required on the cluster.

## 6. Branching & milestones

| ms | content | depends |
|---|---|---|
| M0 | Rebase PR #558 onto `appmana-extras` branch. CI green. | upstream |
| M1 | **CSI driver scaffolding**: split main.go into controller/node modes; add CSIDriver + Deployment + DaemonSet manifests with sidecars; implement Identity, ControllerCreateVolume/DeleteVolume, NodeStage/Publish/Unstage/Unpublish. Reuse existing helper-pod machinery for create/delete. Drop `sigs.k8s.io/sig-storage-lib-external-provisioner` from go.mod. | M0 |
| M2 | **CSIStorageCapacity**: Node-mode loop publishes free-bytes Node annotations; Controller GetCapacity reads them; csi-provisioner sidecar runs with `--enable-capacity`. e2e: 3 nodes with skewed free-bytes, verify scheduler picks the freest. | M1 |
| M3 | **Resize**: ControllerExpandVolume + NodeExpandVolume RPCs; csi-resizer sidecar; helper script `resize` reused from PR #558. Drop annotation-driven shrink. | M1 |
| M4 | **ext4 + btrfs branches** in helper scripts. New e2e dirs `quota-ext4-*`, `quota-btrfs-*` mirroring `quota-xfs-*`. | M1 |
| M5 | **Multi-platform helper image** in this fork: `package/helper-image/{linux,windows}.Dockerfile` + scripts + `.github/workflows/helper-image.yml`. Linux multi-arch via buildx; Windows on `windows-2022` runner with servercore base; `crane index append` combines into one manifest list pushed to GHCR. | M4 |
| M6 | **NTFS + ReFS branches** (Windows scripts) + Windows DaemonSet variant. e2e gated to a Windows-equipped cluster. | M5 |
| M7 | **VolumeSnapshot CRDs**: ControllerCreateSnapshot/DeleteSnapshot/ListSnapshots; csi-snapshotter sidecar; per-FS snapshot scripts; node-local sidecar JSON. | M4 |
| M8 | **Restore from snapshot** via `VolumeContentSource`. e2e: snapshot → delete PVC → restore PVC, data round-trips. | M7 |
| M9 | Helm chart finalization, README, migration notes from PR #558 (XFS-only) and from upstream local-path-provisioner. | M3, M6, M8 |

## 7. Implementation notes (decided)

- **Driver name**: `local-path.appmana.io`.
- **Controller binary base image**: `gcr.io/distroless/static:nonroot` (kubernetes-csi convention).
- **Windows helper image base**: `mcr.microsoft.com/windows/servercore:ltsc2022`. Size (~5GB) accepted.
- **CSIStorageCapacity poll cadence**: 30s, configurable via csi-provisioner sidecar `--capacity-poll-interval`.
- **Controller binary registry**: GHCR (`ghcr.io/appmana/local-path-csi`). Cluster's existing image-replication mirrors to Harbor.
- **ext4 detection**: use `findmnt -no FSTYPE "$parentDir"`, not `stat -f -c %T` (which reports ext4 as `ext2/ext3`).
- **xfs reflink probe**: `xfs_info "$mp" | grep -q 'reflink=1'`. Reject `CreateSnapshot` with `codes.FailedPrecondition` if absent.
- **btrfs snapshot qgroup**: independent qgroup, sized to source's allocated bytes at snapshot time. Matches CSI semantics where `Snapshot.SizeBytes` is the source size at snapshot creation.

## 8. Local dev clusters

- **Linux-only iteration**: `kind` on this machine. Fast container-based, no VM overhead. Used for CSI build → apply → e2e dev loop in M1–M4, M7–M8.
- **Linux + Windows iteration**: `kubernetes-sigs/sig-windows-dev-tools` — Vagrant + VirtualBox, kubeadm control plane + Linux worker + Windows Server 2019/2022 worker, Calico or Antrea. Active repo. Used for M5–M6 (Windows helper image, NTFS/ReFS quota and snapshot scripts).
  ```sh
  vagrant plugin install vagrant-reload vagrant-vbguest winrm winrm-elevated
  git clone https://github.com/kubernetes-sigs/sig-windows-dev-tools && cd sig-windows-dev-tools
  make all
  vagrant ssh controlplane -- kubectl get nodes
  ```
- **Production-representative Windows e2e**: `appmana-cluster-03`. The sig-windows-dev-tools stack uses containerd 1.6 + Calico defaults, while production runs containerd 2.x + cimfs + Calico-windows with DSR disabled. For final validation of Windows-side features, deploy the CSI driver to a feature namespace on `appmana-cluster-03`. Local sig-windows-dev-tools is the dev loop; cluster-03 is the validation gate.

## 9. Out of scope

- ZFS, LVM-thin, dm-thin (use TopoLVM / openebs-lvm-localpv for those).
- Cross-node snapshot replication (btrfs send/receive, robocopy across nodes).
- PVC shrink (CSI doesn't standardize it; PR #558's annotation-driven shrink is dropped here).
- Automatic remount with `prjquota`/`pquota` — operator's responsibility at host setup time.
