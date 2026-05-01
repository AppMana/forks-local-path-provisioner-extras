//go:build sanity

package csi_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"

	lpcsi "github.com/rancher/local-path-provisioner/csi"
	"github.com/rancher/local-path-provisioner/internal/lpp"
)

// pvShadow is an interceptor that simulates the csi-provisioner sidecar:
// after a successful CreateVolume it materializes the PV in the fake
// clientset, and after DeleteVolume it removes it. This lets sanity specs
// like ValidateVolumeCapabilities (which we make conditional on PV
// existence per CSI spec) see the volume.
func pvShadow(kc *fake.Clientset) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			return resp, err
		}
		switch r := resp.(type) {
		case *csi.CreateVolumeResponse:
			if r.Volume != nil {
				_, _ = kc.CoreV1().PersistentVolumes().Create(ctx, &v1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: r.Volume.VolumeId},
					Spec: v1.PersistentVolumeSpec{
						Capacity:                      v1.ResourceList{v1.ResourceStorage: *resource.NewQuantity(r.Volume.CapacityBytes, resource.BinarySI)},
						PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimDelete,
						PersistentVolumeSource: v1.PersistentVolumeSource{
							CSI: &v1.CSIPersistentVolumeSource{
								Driver:           lpcsi.DriverName,
								VolumeHandle:     r.Volume.VolumeId,
								VolumeAttributes: r.Volume.VolumeContext,
							},
						},
					},
				}, metav1.CreateOptions{})
			}
		case *csi.DeleteVolumeResponse:
			if dr, ok := req.(*csi.DeleteVolumeRequest); ok && dr.VolumeId != "" {
				_ = kc.CoreV1().PersistentVolumes().Delete(ctx, dr.VolumeId, metav1.DeleteOptions{})
			}
		}
		return resp, nil
	}
}

const sanityHelperPodYaml = `
apiVersion: v1
kind: Pod
metadata:
  name: helper-pod
spec:
  containers:
    - name: helper-pod
      image: busybox
`

// Sanity uses sharedFileSystemPath so the controller doesn't require
// AccessibilityRequirements on every CreateVolume — sanity's request builder
// doesn't populate topology by default.
const sanityConfigJSON = `{"sharedFileSystemPath": "/tmp/csi-sanity-data"}`

// TestSanity runs the kubernetes-csi/csi-test compliance suite against an
// in-process gRPC server backed by a fake clientset and a no-op helper-pod
// runner. Build with `-tags=sanity` to enable.
func TestSanity(t *testing.T) {
	dir := t.TempDir()
	endpoint := "unix://" + filepath.Join(dir, "csi.sock")
	target := filepath.Join(dir, "target")
	staging := filepath.Join(dir, "staging")

	kc := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov, err := lpp.NewProvisioner(ctx, kc, sanityConfigJSON, "ns", "img", "lpp-cm", "lpp-sa", sanityHelperPodYaml)
	if err != nil {
		t.Fatalf("provisioner: %v", err)
	}
	prov.SetRunHelperPodFn(func(_ context.Context, _ lpp.HelperAction) error { return nil })

	// Start the gRPC server on the UDS endpoint.
	scheme, addr, err := func() (string, string, error) {
		// re-parse the endpoint locally because parseEndpoint is unexported.
		const prefix = "unix://"
		return "unix", endpoint[len(prefix):], nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	_ = scheme
	_ = os.Remove(addr)
	lis, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(pvShadow(kc)))
	csi.RegisterIdentityServer(srv, lpcsi.NewIdentityServer())
	csi.RegisterControllerServer(srv, lpcsi.NewControllerServer(prov))
	nodeSrv := lpcsi.NewNodeServer("test-node")
	// Use the fake mounter so bind-mount-flavored sanity specs don't need
	// CAP_SYS_ADMIN. The fake mounter simulates Mount/Unmount in memory.
	nodeSrv.SetMounter(mount.NewFakeMounter(nil))
	csi.RegisterNodeServer(srv, nodeSrv)
	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	cfg := sanity.NewTestConfig()
	cfg.Address = endpoint
	cfg.TargetPath = target
	cfg.StagingPath = staging
	cfg.TestVolumeSize = 1 << 20
	cfg.TestVolumeParameters = map[string]string{
		lpcsi.ParamPVCName:      "sanity-pvc",
		lpcsi.ParamPVCNamespace: "default",
	}
	// Fake clientset does not auto-create PVs, so DeleteVolume's PV-lookup
	// path returns NotFound and is handled idempotently. Sanity is happy.
	sanity.Test(t, cfg)
}
