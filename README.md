# Local Path Provisioner — CSI fork (appmana-extras)

This is `AppMana/forks-local-path-provisioner-extras`: a CSI rewrite of
`rancher/local-path-provisioner` with per-filesystem quotas, snapshots,
multi-OS dispatch, and capacity-aware scheduling. The upstream `master`
branch's sig-storage-lib provisioner has been replaced by a real CSI
driver (controller + node DaemonSet) so kubelet can publish
`CSIStorageCapacity`, expand volumes through `ControllerExpandVolume`,
and back snapshots with `VolumeSnapshot`/`VolumeSnapshotContent`.

## Overview

The driver provisions a `hostPath`-style PV on the selected node, runs
short-lived helper pods to do the per-FS work (create directory, set
quota, take snapshot, restore from snapshot, expand), and reports
free-byte capacity to the scheduler so PVCs land on a node that can
actually accommodate them. See [`docs/hook-scripts.md`](docs/hook-scripts.md)
for the helper-pod contract and [`docs/snapshots.md`](docs/snapshots.md)
for the per-FS snapshot mechanics.

## Compare to built-in Local Persistent Volume feature in Kubernetes

### Pros
Dynamic provisioning the volume using [hostPath](https://kubernetes.io/docs/concepts/storage/volumes/#hostpath) or [local](https://kubernetes.io/docs/concepts/storage/volumes/#local).
* Currently the Kubernetes [Local Volume provisioner](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner) cannot do dynamic provisioning for the local volumes.
* Local based persistent volumes are an experimental feature ([example usage](examples/pvc-with-local-volume/pvc.yaml)).

### Volume Capacity Limits

The provisioner supports three types of volume capacity enforcement:

1. **Per-PVC Size Validation** (provisioning-time check)
   Rejects PVC creation if the requested storage falls outside configured `minSize` / `maxSize` bounds. Configured via StorageClass `.parameters` or `config.json` `storageClassConfigs`. No special host requirements.

2. **Per-Path Capacity Budget** (provisioning-time check)
   Tracks total allocated bytes per `(node, path)` pair and rejects new PVCs that would exceed a path's `maxCapacity`. Configured in `config.json` `nodePathMap` using the object path format `{"path": "/mnt/data", "maxCapacity": "100Gi"}`. No special host requirements.

3. **Filesystem-Level Quota Enforcement** (runtime enforcement via per-FS quotas)
   Hard-limits the amount of data a pod can write to its PVC volume. Writes
   beyond the PVC's requested size return `ENOSPC`. The shipped helper image
   covers xfs project quotas, ext4 project quotas, btrfs qgroups on Linux,
   and FSRM hard quotas on Windows NTFS. Configured via
   `quotaEnforcement: auto | xfs | ext4 | btrfs | ntfs | refs | none` on the
   StorageClass. `auto` detects the filesystem at the configured node path.

   **Requirements for filesystem-level enforcement on Linux:**
   - xfs needs the volume's parent path on an xfs FS mounted with `prjquota`.
   - ext4 needs `prjquota` mount option + `chattr +P` support (kernel 4.5+).
   - btrfs needs qgroups enabled on the subvolume.
   - Host must have `/etc/projects` and `/etc/projid` files (created via `touch`).
   - The helper pod runs privileged with bind-mounts on those files.

   **Windows:**
   - The node must have the FS-Resource-Manager Windows feature installed
     (`Install-WindowsFeature FS-Resource-Manager -IncludeManagementTools`).
   - The `nodePath` MUST NOT live under `C:\Windows\…` or `C:\Program Files\…` —
     FSRM silently demotes hard quotas to soft on system paths. See
     [`docs/windows-fsrm.md`](docs/windows-fsrm.md).

## Requirement
Kubernetes v1.24+. Snapshots require the external-snapshotter CRDs +
snapshot-controller installed cluster-wide.

## Deployment

### Helm chart (preferred)

```bash
helm install lpp deploy/chart/local-path-csi \
  --namespace local-path-storage --create-namespace
```

Disable snapshots (skips the csi-snapshotter sidecar + VolumeSnapshotClass)
if the external-snapshotter CRDs are not installed:

```bash
helm install lpp deploy/chart/local-path-csi \
  --namespace local-path-storage --create-namespace \
  --set snapshots.enabled=false
```

Enable the Windows node DaemonSet on a mixed cluster:

```bash
helm install lpp deploy/chart/local-path-csi \
  --namespace local-path-storage --create-namespace \
  --set windows.enabled=true
```

### Plain kustomize

```bash
kubectl apply -k deploy/csi
```

The kustomize layer in `deploy/csi/` is what the e2e suite installs; the
Helm chart above renders the same manifests with values knobs and a clean
upgrade story.

After installation:

```bash
kubectl -n local-path-storage get pod
# lpp-local-path-csi-controller-...   3/3   Running
# lpp-local-path-csi-node-...         2/2   Running   (one per Linux node)
```

Controller logs:

```bash
kubectl -n local-path-storage logs -f -l app=lpp-local-path-csi-controller
```

## Usage

Bind a PVC against the shipped `local-path-csi` StorageClass and mount it:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: lpp-pvc, namespace: default }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path-csi
  resources: { requests: { storage: 1Gi } }
---
apiVersion: v1
kind: Pod
metadata: { name: lpp-pod, namespace: default }
spec:
  containers:
    - name: c
      image: busybox:1.36
      command: [sh,-c,'echo hello > /data/x && sleep 600']
      volumeMounts: [{ name: v, mountPath: /data }]
  volumes:
    - name: v
      persistentVolumeClaim: { claimName: lpp-pvc }
```

`kubectl exec lpp-pod -- cat /data/x` returns `hello`. Delete the pod, the
volume content persists on the node; delete the PVC, the helper pod
removes the directory (and any quota state). End-to-end verified in
[`test/csi_test.go`](test/csi_test.go).

## Configuration

### Customize the ConfigMap

The configuration of the provisioner is a json file `config.json`, a Pod template `helperPod.yaml` and two bash scripts `setup` and `teardown`, stored in a config map, e.g.:
```
kind: ConfigMap
apiVersion: v1
metadata:
  name: local-path-config
  namespace: local-path-storage
data:
  config.json: |-
        {
                "nodePathMap":[
                {
                        "node":"DEFAULT_PATH_FOR_NON_LISTED_NODES",
                        "paths":["/opt/local-path-provisioner"]
                },
                {
                        "node":"yasker-lp-dev1",
                        "paths":["/opt/local-path-provisioner", "/data1"]
                },
                {
                        "node":"yasker-lp-dev3",
                        "paths":[]
                }
                ]
        }
  setup: |-
        #!/bin/sh
        set -eu
        mkdir -m 0777 -p "$VOL_DIR"
  teardown: |-
        #!/bin/sh
        set -eu
        rm -rf "$VOL_DIR"
  helperPod.yaml: |-
        apiVersion: v1
        kind: Pod
        metadata:
          name: helper-pod
        spec:
          priorityClassName: system-node-critical
          tolerations:
            - key: node.kubernetes.io/disk-pressure
              operator: Exists
              effect: NoSchedule
          containers:
          - name: helper-pod
            image: busybox

```

The helperPod is allowed to run on nodes experiencing disk pressure conditions, despite the potential resource constraints. When it runs on such a node, it can carry out specific cleanup tasks, freeing up space in PVCs, and resolving the disk-pressure issue.

#### `config.json`

##### Definition
`nodePathMap` is the place user can customize where to store the data on each node.
1. If one node is not listed on the `nodePathMap`, and Kubernetes wants to create volume on it, the paths specified in `DEFAULT_PATH_FOR_NON_LISTED_NODES` will be used for provisioning.
2. If one node is listed on the `nodePathMap`, the specified paths in `paths` will be used for provisioning.
    1. If one node is listed but with `paths` set to `[]`, the provisioner will refuse to provision on this node.
    2. If more than one path was specified, the path would be chosen randomly when provisioning.

`sharedFileSystemPath` allows the provisioner to use a filesystem that is mounted on all nodes at the same time.
In this case all access modes are supported: `ReadWriteOnce`, `ReadOnlyMany` and `ReadWriteMany` for storage claims.

`storageClassConfigs` is a map from storage class names to objects containing `nodePathMap` or `sharedFilesystemPath`, as described above.

In addition `volumeBindingMode: Immediate` can be used in  StorageClass definition.

Please note that `nodePathMap`, `sharedFileSystemPath`, and `storageClassConfigs` are mutually exclusive. If `sharedFileSystemPath` or `storageClassConfigs` are used, then `nodePathMap` must be set to `[]`.

The full helper-pod command contract — every action (`create`, `delete`,
`resize`, `snapshot`, `delete-snapshot`, `restore`, `check-usage`), the
env vars, the `-p / -s / -m / -a` argv shape, stdout conventions, and the
four-level override matrix (per-StorageClass → per-OS → global → built-in
default) — lives in [`docs/hook-scripts.md`](docs/hook-scripts.md).

##### Rules
The configuration must obey following rules:
1. `config.json` must be a valid json file.
2. A path must start with `/`, a.k.a an absolute path.
2. Root directory(`/`) is prohibited.
3. No duplicate paths allowed for one node.
4. No duplicate node allowed.

#### Scripts `setup` and `teardown` and the `helperPod.yaml` template

* The `setup` script is run before the volume is created, to prepare the volume directory on the node.
* The `teardown` script is run after the volume is deleted, to cleanup the volume directory on the node.
* The `helperPod.yaml` template is used to create a helper Pod that runs the `setup` or `teardown` script.

The scripts receive their input as environment variables:

| Environment variable | Description |
| -------------------- | ----------- |
| `VOL_DIR` | Volume directory that should be created or removed. |
| `VOL_MODE` | The PersistentVolume mode (`Block` or `Filesystem`). |
| `VOL_SIZE_BYTES` | Requested volume size in bytes. |

#### Reloading

The provisioner supports automatic configuration reloading. Users can change the configuration using `kubectl apply` or `kubectl edit` with config map `local-path-config`. There is a delay between when the user updates the config map and the provisioner picking it up. In order for this to occur for updates made to the helper pod manifest, the following environment variable must be added to the provisioner container. If not, then the manifest used for the helper pod will be the same as what was in the config map when the provisioner was last restarted/deployed.

```yaml
- name: CONFIG_MOUNT_PATH
  value: /etc/config/
```

When the provisioner detects the configuration changes, it will try to load the new configuration. Users can observe it in the log
>time="2018-10-03T05:56:13Z" level=debug msg="Applied config: {\"nodePathMap\":[{\"node\":\"DEFAULT_PATH_FOR_NON_LISTED_NODES\",\"paths\":[\"/opt/local-path-provisioner\"]},{\"node\":\"yasker-lp-dev1\",\"paths\":[\"/opt\",\"/data1\"]},{\"node\":\"yasker-lp-dev3\"}]}"

If the reload fails, the provisioner will log the error and **continue using the last valid configuration for provisioning in the meantime**.
>time="2018-10-03T05:19:25Z" level=error msg="failed to load the new config file: fail to load config file /etc/config/config.json: invalid character '#' looking for beginning of object key string"

>time="2018-10-03T05:20:10Z" level=error msg="failed to load the new config file: config canonicalization failed: path must start with / for path opt on node yasker-lp-dev1"

>time="2018-10-03T05:23:35Z" level=error msg="failed to load the new config file: config canonicalization failed: duplicate path /data1 on node yasker-lp-dev1

>time="2018-10-03T06:39:28Z" level=error msg="failed to load the new config file: config canonicalization failed: duplicate node yasker-lp-dev3"

### Volume Types

To specify the type of volume you want the provisioner to create, add either of the following annotations;

- PVC:
```yaml
annotations:
  volumeType: <local or hostPath>
```

- StorageClass:
```yaml
annotations:
  defaultVolumeType: <local or hostPath>
```

A few things to note; the annotation for the `StorageClass` will apply to all volumes using it and is superseded by the annotation on the PVC if one is provided. If neither of the annotations was provided then we default to `hostPath`.

### Storage classes

If more than one `paths` are specified in the `nodePathMap` the path is chosen randomly. To make the provisioner choose a specific path, use a `storageClass` defined with a parameter called `nodePath`. Note that this path should be defined in the `nodePathMap`.

By default the volume subdirectory is named using the template `{{ .PVName }}_{{ .PVC.Namespace }}_{{ .PVC.Name }}` which make the directory specific to the PV instance. The template can be changed using the `pathPattern` parameter which is interpreted as a go template. The template has access to the PV name using the `PVName` variable and the PVC metadata object, including labels and annotations, with the `PVC` variable.

When `pathPattern` is set, the rendered path must start with `{{ .PVC.Namespace }}/{{ .PVC.Name }}/` and must not contain directory traversal (for example `../`).

If you need to keep an existing `pathPattern` that does not follow the prefix requirement, you can opt out by setting `allowUnsafePathPattern: "true"` on the StorageClass (either in `parameters` or `metadata.annotations`). When enabled, the provisioner will skip these validations.
```
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ssd-local-path
provisioner: local-path.appmana.com
parameters:
  nodePath: /data/ssd
  pathPattern: "{{ .PVC.Namespace }}/{{ .PVC.Name }}/"
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
```

Here the provisioner will use the path `/data/ssd` with a subdirectory per namespace and PVC when storage class `ssd-local-path` is used.

### Node Affinity Key

By default, PersistentVolumes created by the provisioner use the `kubernetes.io/hostname` label for node affinity. This ensures the volume is only accessible on the node where it was provisioned.

In environments where node hostnames are not stable (e.g., VMs managed by orchestrators that assign new hostnames on recreation), the PV node affinity can reference a stale hostname, making the volume unschedulable. The `nodeAffinityKey` StorageClass parameter allows you to specify a different node label key that remains stable across node restarts or recreation.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: local-path-stable
provisioner: local-path.appmana.com
parameters:
  nodeAffinityKey: my.domain/stable-node-id
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
```

When using this parameter, make sure every node in the cluster has the specified label set. The provisioner reads the label value from the selected node at provisioning time and writes it into the PV's `nodeAffinity` field.

If the parameter is not specified, the default `kubernetes.io/hostname` behavior is preserved.

**Upgrade note:** Existing PVs created before adding `nodeAffinityKey` retain their original `kubernetes.io/hostname` affinity. The provisioner handles deletion of both old and new PVs transparently. Only newly provisioned PVs will use the custom key.

## Uninstall

Before uninstallation, delete every PV created by the driver
(`kubectl get pv` and check that no PV references the StorageClass).
Then:

```bash
helm uninstall lpp -n local-path-storage
```

or with kustomize:

```bash
kubectl delete -k deploy/csi
```

`helm uninstall` removes the CSIDriver, ClusterRoles, RBAC bindings, the
StorageClass, the namespace's resources, and (when `snapshots.enabled=true`)
the VolumeSnapshotClass. Verified clean on the kind e2e cluster.

## Further docs

- [`docs/hook-scripts.md`](docs/hook-scripts.md) — the contract every helper-pod
  script honors (env vars, args, exit codes), the override matrix, and the
  table of built-in defaults per filesystem.
- [`docs/snapshots.md`](docs/snapshots.md) — per-filesystem snapshot mechanics:
  btrfs / xfs reflink on Linux, VSS+mklink on NTFS, block clone on ReFS.
- [`docs/windows-fsrm.md`](docs/windows-fsrm.md) — FSRM hard quotas only
  enforce on non-system paths; operator note for picking a Windows `nodePath`.
- [`docs/windows-e2e.md`](docs/windows-e2e.md) — qemu Windows Server 2022 lab
  VM bringup for snapshot / quota / resize iteration.
- [`docs/plan.md`](docs/plan.md) — implementation plan for the CSI fork.

## License

Copyright (c) 2014-2020  [Rancher Labs, Inc.](http://rancher.com/)

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the License. You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific language governing permissions and limitations under the License.
