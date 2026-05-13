#!/usr/bin/env bash
# windows-e2e-iteration.sh — one PVC-lifecycle iteration against the qemu
# Windows VM. Restores disk-baseline-lpp.qcow2, boots, starts kubelet,
# applies + verifies + tears down a PVC + Pod on the Windows node.
#
# Pre-conditions:
#   * bash test/windows/create-windows-baseline.sh has been run.
#   * A Linux control plane the VM can reach (kind cluster from
#     test/windows/kind-cluster-windows-cp.yaml is the recommended setup).
#   * $KUBECONFIG points at that cluster.
set -euo pipefail

APPMANA_MANAGEMENT=${APPMANA_MANAGEMENT:-$HOME/Documents/appmana/appmana-management/src/appmana_management}
WIN_AUTOINSTALL=${WIN_AUTOINSTALL:-$APPMANA_MANAGEMENT/autoinstall/windows}
WORK=${WORK:-/var/tmp/appmana-winauto-vm}
VM_IP=${VM_IP:-10.2.0.180}
WIN_NODE=${WIN_NODE:-appmana-qemu-win}
NS=${NS:-csi-win-e2e}
PVC=${PVC:-data}
POD=${POD:-writer}

note() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

[[ -f "$WORK/disk-baseline-lpp.qcow2" ]] || die "no LPP baseline; run create-windows-baseline.sh first"
command -v kubectl >/dev/null || die "kubectl not in PATH"
[[ -n "${KUBECONFIG:-}" ]] || die "KUBECONFIG not set; must point at the control plane the VM joins"

# Kill any prior VM, restore the LPP baseline, boot.
[[ -f "$WORK/qemu.pid" ]] && kill -0 "$(cat "$WORK/qemu.pid")" 2>/dev/null && kill -9 "$(cat "$WORK/qemu.pid")" || true
cp -f "$WORK/disk-baseline-lpp.qcow2" "$WORK/disk.qcow2"
note "Booting from LPP baseline"
USE_BASELINE=1 bash "$WIN_AUTOINSTALL/vm-test.sh" >/dev/null

note "Waiting for WinRM on $VM_IP (max 5 min)"
for _ in $(seq 1 30); do nc -z -w 2 "$VM_IP" 5985 && break; sleep 10; done
nc -z -w 2 "$VM_IP" 5985 || die "WinRM never came up"

note "Starting kubelet"
ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no "administrator@$VM_IP" 'nssm start kubelet' \
    || die "nssm start kubelet failed"

note "Waiting for node $WIN_NODE to be Ready (max 3 min)"
for _ in $(seq 1 36); do
    kubectl get node "$WIN_NODE" --no-headers 2>/dev/null | awk '{print $2}' | grep -q '^Ready$' && break
    sleep 5
done
kubectl get node "$WIN_NODE" --no-headers | awk '{print $2}' | grep -q '^Ready$' || die "node never reached Ready"

note "Applying the Windows DaemonSet"
ssh "administrator@$VM_IP" 'kubectl apply -f C:\opt\local-path-csi\node-windows.yaml' \
    || kubectl apply -f "$(dirname "$0")/../../deploy/csi/node-windows.yaml"
kubectl -n local-path-storage rollout status daemonset/local-path-csi-node-windows --timeout=120s

note "Applying PVC + Pod (pinned to the Windows node)"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
cat <<YAML | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: $PVC, namespace: $NS }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path-csi
  resources: { requests: { storage: 1Gi } }
---
apiVersion: v1
kind: Pod
metadata: { name: $POD, namespace: $NS }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/os: windows }
  containers:
    - name: c
      image: mcr.microsoft.com/windows/nanoserver:ltsc2022
      command: ["cmd", "/c", "ping -t 127.0.0.1 >NUL"]
      volumeMounts:
        - name: data
          mountPath: C:/data
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: $PVC }
YAML
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}=Bound' "pvc/$PVC" --timeout=120s
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s

note "Writing + reading a file through the bound volume"
kubectl -n "$NS" exec "$POD" -- cmd /c "echo hello-csi-win > C:\data\marker.txt"
out=$(kubectl -n "$NS" exec "$POD" -- cmd /c "type C:\data\marker.txt")
[[ "$out" == *"hello-csi-win"* ]] || die "marker read mismatch: $out"

note "Resolving the on-node volume path for cleanup verification"
pvname=$(kubectl -n "$NS" get pvc "$PVC" -o jsonpath='{.spec.volumeName}')
node_path=$(kubectl get pv "$pvname" -o jsonpath='{.spec.csi.volumeAttributes.path}')
note "  pv=$pvname  path=$node_path"

note "Tearing down Pod + PVC"
kubectl -n "$NS" delete pod "$POD" --wait=true
kubectl -n "$NS" delete pvc "$PVC" --wait=true
kubectl delete namespace "$NS" --wait=true

note "Verifying the on-disk directory was removed on the VM"
left=$(ssh "administrator@$VM_IP" "powershell -NoProfile -Command \"Test-Path '$node_path'\"")
case "$left" in
    *False*) note "PASS: volume directory cleaned up" ;;
    *)       die "FAIL: directory $node_path still exists on the Windows node ($left)" ;;
esac

note "Stopping kubelet (leaves the VM ready for the next iteration)"
ssh "administrator@$VM_IP" 'nssm stop kubelet' || true
note "PASS"
