package csi

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rancher/local-path-provisioner/internal/lpp"
)

const helperPodYaml = `
apiVersion: v1
kind: Pod
metadata:
  name: helper-pod
spec:
  containers:
    - name: helper-pod
      image: busybox
`

// fixture builds a controller server backed by a fake clientset and a recording
// helper-pod runner. Returns the server, the kube client, and a slice that
// receives every helper action that was dispatched.
type fixture struct {
	cs          *ControllerServer
	prov        *lpp.Provisioner
	kc          *fake.Clientset
	helperCalls *[]lpp.HelperAction
	mu          *sync.Mutex
}

func newFixture(t *testing.T, configJSON string, prepop ...runtime.Object) *fixture {
	t.Helper()
	kc := fake.NewSimpleClientset(prepop...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Inline JSON skips the watch goroutine (see lpp.NewProvisioner).
	prov, err := lpp.NewProvisioner(ctx, kc, configJSON, "ns", "img", "lpp-cm", "lpp-sa", helperPodYaml)
	require.NoError(t, err)

	calls := []lpp.HelperAction{}
	mu := &sync.Mutex{}
	prov.SetRunHelperPodFn(func(_ context.Context, a lpp.HelperAction) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, a)
		return nil
	})

	return &fixture{
		cs:          NewControllerServer(prov),
		prov:        prov,
		kc:          kc,
		helperCalls: &calls,
		mu:          mu,
	}
}

func (f *fixture) calls() []lpp.HelperAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]lpp.HelperAction, len(*f.helperCalls))
	copy(out, *f.helperCalls)
	return out
}

// validCreateReq builds a CreateVolumeRequest the controller can handle.
func validCreateReq(name string, bytes int64, node string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          name,
		CapacityRange: &csi.CapacityRange{RequiredBytes: bytes},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters: map[string]string{
			ParamPVCName:      "data-pvc",
			ParamPVCNamespace: "default",
			ParamPVName:       name,
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: node}}},
		},
	}
}

func TestControllerServer_CreateVolume_HappyPath(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	resp, err := f.cs.CreateVolume(context.Background(), validCreateReq("pv-1", 1<<30, "n1"))
	require.NoError(t, err)
	require.NotNil(t, resp.Volume)
	assert.Equal(t, "pv-1", resp.Volume.VolumeId)
	assert.Equal(t, int64(1<<30), resp.Volume.CapacityBytes)
	assert.Equal(t, "n1", resp.Volume.VolumeContext[VolumeContextNode])
	assert.Equal(t, "/data/pv-1_default_data-pvc", resp.Volume.VolumeContext[VolumeContextPath])
	require.Len(t, resp.Volume.AccessibleTopology, 1)
	assert.Equal(t, "n1", resp.Volume.AccessibleTopology[0].Segments[TopologyKeyNode])

	calls := f.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, lpp.ActionTypeCreate, calls[0].Type)
	assert.Equal(t, "n1", calls[0].Volume.Node)
	assert.Equal(t, int64(1<<30), calls[0].Volume.SizeInBytes)
}

func TestControllerServer_CreateVolume_MissingName(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	req := validCreateReq("pv", 1<<30, "n1")
	req.Name = ""
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestControllerServer_CreateVolume_MissingPVCMetadata(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	req := validCreateReq("pv-1", 1<<30, "n1")
	delete(req.Parameters, ParamPVCName)
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "extra-create-metadata")
}

func TestControllerServer_CreateVolume_NoPreferredTopology(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	req := validCreateReq("pv-1", 1<<30, "n1")
	req.AccessibilityRequirements = nil
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "preferred topology")
}

func TestControllerServer_CreateVolume_BelowMinSize(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}],"minSize":"1Gi"}`)
	req := validCreateReq("pv-small", 1<<20, "n1") // 1MiB, below 1Gi
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.OutOfRange, status.Code(err))
}

func TestControllerServer_CreateVolume_AboveMaxSize(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}],"maxSize":"1Gi"}`)
	req := validCreateReq("pv-big", 10<<30, "n1") // 10GiB > 1Gi
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.OutOfRange, status.Code(err))
}

func TestControllerServer_CreateVolume_StorageClassParamOverridesMinMax(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}],"maxSize":"1Gi"}`)
	req := validCreateReq("pv-big", 5<<30, "n1") // 5GiB
	req.Parameters["maxSize"] = "10Gi"           // override allows
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.NoError(t, err)
}

func TestControllerServer_CreateVolume_CapacityExhausted(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":[{"path":"/data","maxCapacity":"1Gi"}]}]}`)
	req := validCreateReq("pv-big", 2<<30, "n1") // 2GiB > 1Gi cap
	req.Parameters["nodePath"] = "/data"
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestControllerServer_CreateVolume_BadAccessMode(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	req := validCreateReq("pv-1", 1<<30, "n1")
	req.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestControllerServer_CreateVolume_HelperPodFails_ReleasesCapacity(t *testing.T) {
	cfgJSON := `{"nodePathMap":[{"node":"n1","paths":[{"path":"/data","maxCapacity":"10Gi"}]}]}`
	f := newFixture(t, cfgJSON)
	calls := atomic.Int64{}
	f.prov.SetRunHelperPodFn(func(_ context.Context, _ lpp.HelperAction) error {
		calls.Add(1)
		return assertableError("helper boom")
	})
	req := validCreateReq("pv-1", 1<<30, "n1")
	req.Parameters["nodePath"] = "/data"
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Equal(t, int64(1), calls.Load())
	// Capacity must have been released — the next allocation of the full
	// budget should succeed.
	assert.Equal(t, int64(0), f.prov.CapacityTracker().GetAllocated("n1", "/data"))
}

func TestControllerServer_DeleteVolume_HappyPath(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "1Gi", v1.PersistentVolumeReclaimDelete)
	node := makeNode("n1")
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv, node)
	resp, err := f.cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pv-1"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	calls := f.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, lpp.ActionTypeDelete, calls[0].Type)
	assert.Equal(t, "/data/pv-1", calls[0].Volume.Path)
}

func TestControllerServer_DeleteVolume_PVNotFound_Idempotent(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	resp, err := f.cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "missing-pv"})
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Empty(t, f.calls(), "no helper pod should run when PV is gone")
}

func TestControllerServer_DeleteVolume_RetainPolicySkipsHelper(t *testing.T) {
	pv := makeCSIPV("pv-r", "n1", "/data/pv-r", "1Gi", v1.PersistentVolumeReclaimRetain)
	node := makeNode("n1")
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv, node)
	_, err := f.cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pv-r"})
	require.NoError(t, err)
	assert.Empty(t, f.calls(), "Retain reclaim policy: no helper pod should run")
}

func TestControllerServer_DeleteVolume_NodeGone_ReleasesCapacityNoHelper(t *testing.T) {
	pv := makeCSIPV("pv-x", "n-gone", "/data/pv-x", "1Gi", v1.PersistentVolumeReclaimDelete)
	// initCapacityTracker scans existing PVs and allocates 1Gi to the tracker.
	f := newFixture(t, `{"nodePathMap":[{"node":"n-gone","paths":["/data"]}]}`, pv)
	require.Equal(t, int64(1<<30), f.prov.CapacityTracker().GetAllocated("n-gone", "/data"),
		"sanity: initCapacityTracker should have loaded the existing PV's 1Gi")

	_, err := f.cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pv-x"})
	require.NoError(t, err)
	assert.Empty(t, f.calls(), "node gone: no helper pod should run")
	assert.Equal(t, int64(0), f.prov.CapacityTracker().GetAllocated("n-gone", "/data"),
		"DeleteVolume must release tracker capacity even when node is gone")
}

func TestControllerServer_DeleteVolume_MissingID(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	_, err := f.cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestControllerServer_ValidateVolumeCapabilities(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)

	// Existing PV + supported access mode → confirmed.
	resp, err := f.cs.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "pv-1",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Confirmed)

	// Existing PV + unsupported access mode → unconfirmed with message.
	resp, err = f.cs.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "pv-1",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}},
	})
	require.NoError(t, err)
	require.Nil(t, resp.Confirmed)
	assert.True(t, strings.Contains(resp.Message, "unsupported"))

	// Non-existent PV → NotFound (CSI spec).
	_, err = f.cs.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "missing",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))

	// Missing arguments.
	_, err = f.cs.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{})
	require.Error(t, err)
}

func TestControllerServer_CreateVolume_IdempotentSameSize(t *testing.T) {
	pv := makeCSIPV("pv-existing", "n1", "/data/pv-existing", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	req := validCreateReq("pv-existing", 1<<30, "n1") // same 1Gi as the existing PV
	resp, err := f.cs.CreateVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "pv-existing", resp.Volume.VolumeId)
	assert.Equal(t, int64(1<<30), resp.Volume.CapacityBytes)
	assert.Empty(t, f.calls(), "idempotent CreateVolume must not run another helper pod")
}

func TestControllerServer_CreateVolume_RejectAlreadyExistsDifferentSize(t *testing.T) {
	pv := makeCSIPV("pv-existing", "n1", "/data/pv-existing", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	req := validCreateReq("pv-existing", 5<<30, "n1") // 5Gi != existing 1Gi
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestControllerServer_ControllerGetCapabilities(t *testing.T) {
	cs := NewControllerServer(nil)
	resp, err := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	require.NoError(t, err)
	have := map[csi.ControllerServiceCapability_RPC_Type]bool{}
	for _, c := range resp.Capabilities {
		have[c.GetRpc().Type] = true
	}
	// M1 wires only CREATE_DELETE_VOLUME. EXPAND/GET_CAPACITY/SNAPSHOT
	// are advertised as their milestones (M2/M3/M7) land.
	assert.True(t, have[csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME])
	assert.False(t, have[csi.ControllerServiceCapability_RPC_EXPAND_VOLUME])
	assert.False(t, have[csi.ControllerServiceCapability_RPC_GET_CAPACITY])
	assert.False(t, have[csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT])
}

func TestControllerServer_PendingMilestones_ReturnUnimplemented(t *testing.T) {
	cs := NewControllerServer(nil)
	cases := []func() error{
		func() error {
			_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{})
			return err
		},
		func() error { _, err := cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{}); return err },
		func() error {
			_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{})
			return err
		},
		func() error {
			_, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{})
			return err
		},
		func() error { _, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{}); return err },
	}
	for i, fn := range cases {
		err := fn()
		assert.Equal(t, codes.Unimplemented, status.Code(err), "case %d", i)
	}
}

func TestControllerServer_ControllerPublishUnpublish_Noop(t *testing.T) {
	cs := NewControllerServer(nil)
	_, err := cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{})
	require.NoError(t, err)
	_, err = cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{})
	require.NoError(t, err)
}

func TestPickPreferredNode(t *testing.T) {
	got := pickPreferredNode(nil)
	assert.Equal(t, "", got)

	got = pickPreferredNode(&csi.TopologyRequirement{
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "best"}}},
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "fallback"}}},
	})
	assert.Equal(t, "best", got)

	got = pickPreferredNode(&csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "only"}}},
	})
	assert.Equal(t, "only", got)

	got = pickPreferredNode(&csi.TopologyRequirement{
		Preferred: []*csi.Topology{{Segments: map[string]string{"other-key": "x"}}},
	})
	assert.Equal(t, "", got)
}

// makeCSIPV builds a CSI-style PV the way our controller emits one.
func makeCSIPV(name, node, path, size string, reclaim v1.PersistentVolumeReclaimPolicy) *v1.PersistentVolume {
	mode := v1.PersistentVolumeFilesystem
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			Capacity:                      v1.ResourceList{v1.ResourceStorage: resource.MustParse(size)},
			PersistentVolumeReclaimPolicy: reclaim,
			VolumeMode:                    &mode,
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       DriverName,
					VolumeHandle: name,
					VolumeAttributes: map[string]string{
						VolumeContextNode: node,
						VolumeContextPath: path,
					},
				},
			},
		},
	}
}

func makeNode(name string) *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// assertableError lets us produce a typed error in tests.
type assertableError string

func (e assertableError) Error() string { return string(e) }
