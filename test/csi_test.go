//go:build e2e

// CSI driver e2e against a kind cluster.
//
// Build/run:
//
//	# 1. Build the images locally (or pull from ghcr).
//	docker build -t local-path-csi:e2e -f Dockerfile.runtime .
//	docker build -t local-path-helper:e2e -f package/helper-image/linux.Dockerfile package/helper-image
//
//	# 2. Run the suite. The images are loaded into kind by SetupSuite.
//	CSI_IMAGE=local-path-csi:e2e \
//	HELPER_IMAGE=local-path-helper:e2e \
//	go test -tags=e2e -v -timeout=15m ./test/
//
// SetupSuite:  kind create + kind load + kubectl apply -k deploy/csi/ +
//              wait for the controller Deployment and node DaemonSet to
//              be Ready.
// TearDownSuite: kind delete cluster.
//
// Each test creates its own namespace + PVC + Pod and tears them down on exit.

package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const (
	defaultCSIImage    = "local-path-csi:e2e"
	defaultHelperImage = "local-path-helper:e2e"
)

type CSISuite struct {
	suite.Suite

	csiImage    string
	helperImage string
	repoRoot    string
}

func TestCSIE2E(t *testing.T) {
	suite.Run(t, new(CSISuite))
}

func (s *CSISuite) SetupSuite() {
	s.csiImage = envOr("CSI_IMAGE", defaultCSIImage)
	s.helperImage = envOr("HELPER_IMAGE", defaultHelperImage)
	wd, err := os.Getwd()
	require.NoError(s.T(), err)
	s.repoRoot = filepath.Dir(wd)

	s.T().Logf("CSI_IMAGE=%s HELPER_IMAGE=%s repo=%s", s.csiImage, s.helperImage, s.repoRoot)

	// Best-effort: nuke any previous cluster.
	_, _ = runCmd(s.T(), "kind delete cluster 2>/dev/null || true", "", nil, nil)

	bootstrap := []string{
		fmt.Sprintf("kind create cluster --config=%s --wait=120s",
			filepath.Join(s.repoRoot, "test", "testdata", "kind-cluster.yaml")),
		fmt.Sprintf("kind load docker-image %s", s.csiImage),
		fmt.Sprintf("kind load docker-image %s", s.helperImage),
		// Patch the kustomize image refs to the loaded local tags.
		fmt.Sprintf("cd %s && kustomize edit set image ghcr.io/appmana/local-path-csi=%s ghcr.io/appmana/local-path-helper=%s",
			filepath.Join(s.repoRoot, "deploy", "csi"), s.csiImage, s.helperImage),
		// VolumeSnapshotClass is part of deploy/csi/ but the external-snapshotter
		// CRDs aren't installed in this minimal kind cluster; drop it.
		fmt.Sprintf("sed -i '/volumesnapshotclass.yaml/d' %s",
			filepath.Join(s.repoRoot, "deploy", "csi", "kustomization.yaml")),
		fmt.Sprintf("kubectl apply -k %s", filepath.Join(s.repoRoot, "deploy", "csi")),
		"kubectl -n local-path-storage rollout status deployment/local-path-csi-controller --timeout=180s",
		"kubectl -n local-path-storage rollout status daemonset/local-path-csi-node --timeout=180s",
	}
	for _, c := range bootstrap {
		if _, err := runCmd(s.T(), c, "", nil, nil); err != nil {
			s.FailNow("bootstrap step failed", "cmd=%s err=%v", c, err)
		}
	}
}

func (s *CSISuite) TearDownSuite() {
	// Roll the kustomize back to the committed state so subsequent runs aren't
	// poisoned by the SetupSuite mutations.
	_, _ = runCmd(s.T(), "git checkout -- deploy/csi/kustomization.yaml", s.repoRoot, nil, nil)
	_, _ = runCmd(s.T(), "kind delete cluster", "", nil, nil)
}

// TestPVC_Bound_Pod_Mounts is the happy-path lifecycle: create a PVC, mount
// it in a pod, write a file, recreate the pod, read the file back, delete.
func (s *CSISuite) TestPVC_Bound_Pod_Mounts() {
	ns := "csi-happy"
	defer s.cleanupNamespace(ns)
	s.applyYAML(ns, happyPathYAML(ns))

	s.waitForPVCBound(ns, "data", 120*time.Second)
	s.waitForPodReady(ns, "writer", 120*time.Second)
	s.execInPod(ns, "writer", "sh -c 'echo hello-csi > /data/marker.txt'")
	out := s.execInPod(ns, "writer", "cat /data/marker.txt")
	require.Contains(s.T(), out, "hello-csi", "file just written should be readable")

	// Recreate the pod; the PV should still hold the data.
	_, _ = runCmd(s.T(), fmt.Sprintf("kubectl -n %s delete pod writer --wait=true", ns), "", nil, nil)
	s.applyYAML(ns, podOnlyYAML(ns))
	s.waitForPodReady(ns, "writer", 120*time.Second)
	out = s.execInPod(ns, "writer", "cat /data/marker.txt")
	require.Contains(s.T(), out, "hello-csi", "data survives pod recreation")
}

// TestVolumeExpansion: create a 1Gi PVC, patch to 2Gi, observe csi-resizer
// driving ControllerExpandVolume and the PV capacity growing.
func (s *CSISuite) TestVolumeExpansion() {
	ns := "csi-expand"
	defer s.cleanupNamespace(ns)
	s.applyYAML(ns, happyPathYAML(ns))
	s.waitForPVCBound(ns, "data", 120*time.Second)

	_, err := runCmd(s.T(),
		fmt.Sprintf(`kubectl -n %s patch pvc data -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'`, ns),
		"", nil, nil)
	require.NoError(s.T(), err)

	// Wait for the PV capacity to reflect the new size (csi-resizer ->
	// ControllerExpandVolume -> our controller updates the tracker +
	// dispatches resize helper-pod -> csi-resizer patches PV.spec.capacity).
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		c, err := runCmd(s.T(),
			fmt.Sprintf(`kubectl -n %s get pvc data -o jsonpath='{.spec.resources.requests.storage}' && echo ; kubectl get pv $(kubectl -n %s get pvc data -o jsonpath='{.spec.volumeName}') -o jsonpath='{.spec.capacity.storage}'`, ns, ns),
			"", nil, nil)
		_ = c
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	// Spec check: kubectl get pv ... reports 2Gi.
	pv := s.kubectlOutput(fmt.Sprintf(`get pvc data -n %s -o jsonpath='{.spec.volumeName}'`, ns))
	require.NotEmpty(s.T(), pv)
	cap := s.kubectlOutput(fmt.Sprintf(`get pv %s -o jsonpath='{.spec.capacity.storage}'`, pv))
	require.Equal(s.T(), "2Gi", cap, "PV capacity should grow to 2Gi after expansion")
}

// TestCSIStorageCapacity: confirm the csi-provisioner sidecar publishes
// CSIStorageCapacity objects from our GetCapacity RPC.
func (s *CSISuite) TestCSIStorageCapacity() {
	// csi-provisioner --capacity-poll-interval=30s; wait up to 90s for the
	// first publish cycle.
	deadline := time.Now().Add(90 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = s.kubectlOutput(`get csistoragecapacity -A -o jsonpath='{range .items[*]}{.metadata.name} {.storageClassName} {.capacity}{"\n"}{end}'`)
		if strings.Contains(out, "local-path-csi") {
			break
		}
		time.Sleep(5 * time.Second)
	}
	require.Contains(s.T(), out, "local-path-csi",
		"csi-provisioner should publish CSIStorageCapacity objects for our StorageClass within 90s")
}

// --- helpers --------------------------------------------------------------

func (s *CSISuite) applyYAML(ns, body string) {
	tmp, err := os.CreateTemp("", "csi-e2e-*.yaml")
	require.NoError(s.T(), err)
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.WriteString(body)
	require.NoError(s.T(), err)
	_ = tmp.Close()
	_, err = runCmd(s.T(),
		fmt.Sprintf("kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f - && kubectl apply -n %s -f %s", ns, ns, tmp.Name()),
		"", nil, nil)
	require.NoError(s.T(), err)
}

func (s *CSISuite) waitForPVCBound(ns, name string, timeout time.Duration) {
	cmd := fmt.Sprintf("kubectl -n %s wait --for=jsonpath='{.status.phase}'=Bound pvc/%s --timeout=%s", ns, name, timeout)
	_, err := runCmd(s.T(), cmd, "", nil, nil)
	require.NoError(s.T(), err, "PVC %s/%s should bind", ns, name)
}

func (s *CSISuite) waitForPodReady(ns, name string, timeout time.Duration) {
	cmd := fmt.Sprintf("kubectl -n %s wait --for=condition=Ready pod/%s --timeout=%s", ns, name, timeout)
	_, err := runCmd(s.T(), cmd, "", nil, nil)
	require.NoError(s.T(), err, "pod %s/%s should reach Ready", ns, name)
}

func (s *CSISuite) execInPod(ns, name, cmd string) string {
	out := s.kubectlOutput(fmt.Sprintf("-n %s exec %s -- %s", ns, name, cmd))
	return out
}

func (s *CSISuite) kubectlOutput(args string) string {
	cmd := createCmd(s.T(), fmt.Sprintf("kubectl %s", args), "", nil, nil)
	b, err := cmd.CombinedOutput()
	if err != nil {
		s.T().Logf("kubectl %s -> %v: %s", args, err, string(b))
	}
	return strings.TrimSpace(string(b))
}

func (s *CSISuite) cleanupNamespace(ns string) {
	_, _ = runCmd(s.T(), fmt.Sprintf("kubectl delete namespace %s --wait=true --ignore-not-found", ns), "", nil, nil)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func happyPathYAML(ns string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: data, namespace: %s }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path-csi
  resources: { requests: { storage: 1Gi } }
---
apiVersion: v1
kind: Pod
metadata: { name: writer, namespace: %s }
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: busybox:1.36
      command: ["sh", "-c", "sleep 600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: data }
`, ns, ns)
}

func podOnlyYAML(ns string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata: { name: writer, namespace: %s }
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: busybox:1.36
      command: ["sh", "-c", "sleep 600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: data }
`, ns)
}
