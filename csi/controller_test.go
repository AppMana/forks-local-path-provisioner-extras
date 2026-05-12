package csi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// freeBytesNodeAnnotation builds a Node with the capacity reporter's annotation.
func freeBytesNodeAnnotation(name string, free map[string]int64) *v1.Node {
	body, _ := json.Marshal(free)
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				lpp.FreeBytesAnnotationKey: string(body),
			},
		},
	}
}

func TestControllerServer_GetCapacity_NoTopology(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), resp.AvailableCapacity)
}

func TestControllerServer_GetCapacity_NodeMissing(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "ghost"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), resp.AvailableCapacity)
}

func TestControllerServer_GetCapacity_NodeAnnotationMissing(t *testing.T) {
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}} // no free-bytes annotation
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, node)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "n1"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), resp.AvailableCapacity)
}

func TestControllerServer_GetCapacity_HappyPath(t *testing.T) {
	node := freeBytesNodeAnnotation("n1", map[string]int64{"/data": 10 << 30})
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, node)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "n1"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(10)<<30, resp.AvailableCapacity)
	require.NotNil(t, resp.MaximumVolumeSize)
	assert.Equal(t, int64(10)<<30, resp.MaximumVolumeSize.Value)
}

func TestControllerServer_GetCapacity_SubtractsAllocatedFromTracker(t *testing.T) {
	node := freeBytesNodeAnnotation("n1", map[string]int64{"/data": 10 << 30})
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, node)
	f.prov.CapacityTracker().Allocate("n1", "/data", 4<<30)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "n1"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(6)<<30, resp.AvailableCapacity)
	assert.Equal(t, int64(6)<<30, resp.MaximumVolumeSize.Value)
}

func TestControllerServer_GetCapacity_RespectsMaxCapacity(t *testing.T) {
	node := freeBytesNodeAnnotation("n1", map[string]int64{"/data": 100 << 30})
	cfg := `{"nodePathMap":[{"node":"n1","paths":[{"path":"/data","maxCapacity":"5Gi"}]}]}`
	f := newFixture(t, cfg, node)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "n1"}},
	})
	require.NoError(t, err)
	// Filesystem has 100Gi free but maxCapacity caps it to 5Gi.
	assert.Equal(t, int64(5)<<30, resp.AvailableCapacity)
}

func TestControllerServer_GetCapacity_SumsMultiplePaths(t *testing.T) {
	node := freeBytesNodeAnnotation("n1", map[string]int64{
		"/data/a": 3 << 30,
		"/data/b": 7 << 30,
		"/other":  100 << 30, // not configured for this SC, ignored
	})
	cfg := `{"nodePathMap":[{"node":"n1","paths":["/data/a","/data/b"]}]}`
	f := newFixture(t, cfg, node)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "n1"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(10)<<30, resp.AvailableCapacity)
	require.NotNil(t, resp.MaximumVolumeSize)
	assert.Equal(t, int64(7)<<30, resp.MaximumVolumeSize.Value, "MaximumVolumeSize is the largest single path")
}

func TestControllerServer_GetCapacity_NodeNotInConfig(t *testing.T) {
	node := freeBytesNodeAnnotation("ghost", map[string]int64{"/data": 10 << 30})
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, node)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "ghost"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), resp.AvailableCapacity)
}

func TestControllerServer_GetCapacity_DefaultPathFallback(t *testing.T) {
	node := freeBytesNodeAnnotation("anynode", map[string]int64{"/opt/lpp": 5 << 30})
	cfg := `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/opt/lpp"]}]}`
	f := newFixture(t, cfg, node)
	resp, err := f.cs.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "anynode"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5)<<30, resp.AvailableCapacity)
}

func TestControllerServer_ControllerExpandVolume_HappyPath(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	resp, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pv-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5<<30), resp.CapacityBytes)
	assert.False(t, resp.NodeExpansionRequired)
	calls := f.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, lpp.ActionTypeResize, calls[0].Type)
	assert.Equal(t, int64(5<<30), calls[0].Volume.SizeInBytes)
	// Tracker should now hold 5Gi (initCapacityTracker loaded 1Gi, then we resized to 5Gi).
	assert.Equal(t, int64(5<<30), f.prov.CapacityTracker().GetAllocated("n1", "/data"))
}

func TestControllerServer_ControllerExpandVolume_MissingPV(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	_, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "missing",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestControllerServer_ControllerExpandVolume_ShrinkRejected(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "5Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	_, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pv-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}, // shrink 5Gi -> 1Gi
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Empty(t, f.calls(), "rejected expand must not run helper")
}

func TestControllerServer_ControllerExpandVolume_IdempotentAtSize(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	resp, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pv-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1<<30), resp.CapacityBytes)
	assert.False(t, resp.NodeExpansionRequired)
	assert.Empty(t, f.calls(), "idempotent expand must not run helper")
}

func TestControllerServer_ControllerExpandVolume_BudgetExceeded(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":[{"path":"/data","maxCapacity":"2Gi"}]}]}`, pv)
	_, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pv-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30}, // 5Gi > 2Gi cap
	})
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestControllerServer_ControllerExpandVolume_HelperFailureRollsBack(t *testing.T) {
	pv := makeCSIPV("pv-1", "n1", "/data/pv-1", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":[{"path":"/data","maxCapacity":"10Gi"}]}]}`, pv)
	f.prov.SetRunHelperPodFn(func(_ context.Context, _ lpp.HelperAction) error {
		return assertableError("helper boom")
	})
	_, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pv-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30},
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	// Tracker rolled back to the original 1Gi (initCapacityTracker baseline).
	assert.Equal(t, int64(1<<30), f.prov.CapacityTracker().GetAllocated("n1", "/data"))
}

func TestControllerServer_ControllerExpandVolume_MissingArgs(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	_, err := f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = f.cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: "v"})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
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
	// All wired: CREATE_DELETE_VOLUME, EXPAND_VOLUME, GET_CAPACITY,
	// CREATE_DELETE_SNAPSHOT, LIST_SNAPSHOTS.
	assert.True(t, have[csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME])
	assert.True(t, have[csi.ControllerServiceCapability_RPC_EXPAND_VOLUME])
	assert.True(t, have[csi.ControllerServiceCapability_RPC_GET_CAPACITY])
	assert.True(t, have[csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT])
	assert.True(t, have[csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS])
}

func TestControllerServer_SnapshotRPCs_ArgValidation(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "s"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
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

// makeCSIPVWithQuota is like makeCSIPV but stamps a quotaType attribute.
func makeCSIPVWithQuota(name, node, path, size, quotaType string) *v1.PersistentVolume {
	pv := makeCSIPV(name, node, path, size, v1.PersistentVolumeReclaimDelete)
	pv.Spec.PersistentVolumeSource.CSI.VolumeAttributes[VolumeContextQuotaType] = quotaType
	return pv
}

func TestControllerServer_CreateSnapshot_HappyPath(t *testing.T) {
	pv := makeCSIPVWithQuota("pv-src", "n1", "/data/pv-src", "2Gi", "btrfs")
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	resp, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-1",
		SourceVolumeId: "pv-src",
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Snapshot)
	assert.Equal(t, "pv-src/snap-1", resp.Snapshot.SnapshotId)
	assert.Equal(t, "pv-src", resp.Snapshot.SourceVolumeId)
	assert.Equal(t, int64(2)<<30, resp.Snapshot.SizeBytes)
	assert.True(t, resp.Snapshot.ReadyToUse)

	calls := f.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, lpp.ActionTypeSnapshot, calls[0].Type)
	assert.Equal(t, "/data/pv-src", calls[0].Volume.Path)
	assert.Equal(t, "/data/.snapshots/snap-1", calls[0].Volume.SnapDir)
	assert.Equal(t, "n1", calls[0].Volume.Node)

	// In the tracker now.
	info, ok := f.prov.SnapshotTracker().Get("pv-src/snap-1")
	require.True(t, ok)
	assert.Equal(t, "/data/.snapshots/snap-1", info.SnapshotPath)
	assert.Equal(t, "btrfs", info.QuotaType)
}

func TestControllerServer_CreateSnapshot_Idempotent(t *testing.T) {
	pv := makeCSIPV("pv-src", "n1", "/data/pv-src", "2Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pv-src"})
	require.NoError(t, err)
	resp, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pv-src"})
	require.NoError(t, err)
	assert.Equal(t, "pv-src/snap-1", resp.Snapshot.SnapshotId)
	assert.Len(t, f.calls(), 1, "idempotent CreateSnapshot must not re-run the helper")
}

func TestControllerServer_CreateSnapshot_SameNameDifferentSource_AlreadyExists(t *testing.T) {
	pvA := makeCSIPV("pv-a", "n1", "/data/pv-a", "1Gi", v1.PersistentVolumeReclaimDelete)
	pvB := makeCSIPV("pv-b", "n1", "/data/pv-b", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pvA, pvB)
	// CSI spec: snapshot names are globally unique. Create "snap" of pv-a,
	// then "snap" of pv-b must fail with AlreadyExists.
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: "pv-a"})
	require.NoError(t, err)
	_, err = f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: "pv-b"})
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestControllerServer_CreateSnapshot_MissingSourcePV(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "s", SourceVolumeId: "ghost"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestControllerServer_CreateSnapshot_FilesystemUnsupported(t *testing.T) {
	pv := makeCSIPV("pv-ext4", "n1", "/data/pv-ext4", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	f.prov.SetRunHelperPodFn(func(_ context.Context, _ lpp.HelperAction) error {
		return assertableError("snapshots not supported on filesystem ext4")
	})
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "s", SourceVolumeId: "pv-ext4"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestControllerServer_DeleteSnapshot_FromTracker(t *testing.T) {
	pv := makeCSIPV("pv-src", "n1", "/data/pv-src", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "s", SourceVolumeId: "pv-src"})
	require.NoError(t, err)

	_, err = f.cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "pv-src/s"})
	require.NoError(t, err)
	calls := f.calls()
	// 1 snapshot + 1 delete-snapshot.
	require.Len(t, calls, 2)
	assert.Equal(t, lpp.ActionTypeDeleteSnapshot, calls[1].Type)
	assert.Equal(t, "/data/.snapshots/s", calls[1].Volume.SnapDir)
	_, ok := f.prov.SnapshotTracker().Get("pv-src/s")
	assert.False(t, ok, "deleted snapshot must be removed from the tracker")
}

func TestControllerServer_DeleteSnapshot_FromSourcePV_AfterRestart(t *testing.T) {
	// Tracker is empty (simulating a controller restart), but the source PV
	// still exists, so DeleteSnapshot re-derives the path.
	pv := makeCSIPV("pv-src", "n1", "/data/pv-src", "1Gi", v1.PersistentVolumeReclaimDelete)
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	_, err := f.cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "pv-src/orphan"})
	require.NoError(t, err)
	calls := f.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, lpp.ActionTypeDeleteSnapshot, calls[0].Type)
	assert.Equal(t, "/data/.snapshots/orphan", calls[0].Volume.SnapDir)
}

func TestControllerServer_DeleteSnapshot_Idempotent(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	// Unrecognized ID → idempotent success.
	_, err := f.cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "no-slash"})
	require.NoError(t, err)
	// Recognized ID but source PV gone and not in tracker → idempotent success.
	_, err = f.cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "ghost-pv/snap"})
	require.NoError(t, err)
	assert.Empty(t, f.calls())
}

func TestControllerServer_ListSnapshots(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	now := time.Now()
	for _, s := range []lpp.SnapshotInfo{
		{SnapshotID: "pv-a/s1", SourceVolumeID: "pv-a", SizeBytes: 1, CreationTime: now, ReadyToUse: true},
		{SnapshotID: "pv-a/s2", SourceVolumeID: "pv-a", SizeBytes: 2, CreationTime: now, ReadyToUse: true},
		{SnapshotID: "pv-b/s1", SourceVolumeID: "pv-b", SizeBytes: 3, CreationTime: now, ReadyToUse: true},
	} {
		f.prov.SnapshotTracker().Put(s)
	}

	// All.
	resp, err := f.cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	require.NoError(t, err)
	assert.Len(t, resp.Entries, 3)
	assert.Empty(t, resp.NextToken)

	// By snapshot ID.
	resp, err = f.cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: "pv-a/s2"})
	require.NoError(t, err)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "pv-a/s2", resp.Entries[0].Snapshot.SnapshotId)

	// By source volume.
	resp, err = f.cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SourceVolumeId: "pv-a"})
	require.NoError(t, err)
	assert.Len(t, resp.Entries, 2)

	// Pagination.
	resp, err = f.cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{MaxEntries: 2})
	require.NoError(t, err)
	assert.Len(t, resp.Entries, 2)
	require.Equal(t, "2", resp.NextToken)
	resp, err = f.cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: "2"})
	require.NoError(t, err)
	assert.Len(t, resp.Entries, 1)
	assert.Empty(t, resp.NextToken)

	// Bad token.
	_, err = f.cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: "nope"})
	require.Error(t, err)
	assert.Equal(t, codes.Aborted, status.Code(err))
}

func TestControllerServer_CreateVolume_FromSnapshot(t *testing.T) {
	pv := makeCSIPVWithQuota("pv-src", "n1", "/data/pv-src", "1Gi", "btrfs")
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`, pv)
	_, err := f.cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pv-src"})
	require.NoError(t, err)

	req := validCreateReq("pv-restored", 1<<30, "n1")
	req.VolumeContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "pv-src/snap-1"}},
	}
	resp, err := f.cs.CreateVolume(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp.Volume.ContentSource)
	assert.Equal(t, "pv-src/snap-1", resp.Volume.ContentSource.GetSnapshot().GetSnapshotId())

	calls := f.calls()
	// snapshot + restore.
	require.Len(t, calls, 2)
	assert.Equal(t, lpp.ActionTypeRestore, calls[1].Type)
	assert.Equal(t, "/data/.snapshots/snap-1", calls[1].Volume.SnapDir)
	assert.Equal(t, "btrfs", calls[1].Volume.QuotaType, "restored volume inherits the snapshot's quota type when none requested")
}

func TestControllerServer_CreateVolume_FromSnapshot_NotFound(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	req := validCreateReq("pv-restored", 1<<30, "n1")
	req.VolumeContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "ghost/snap"}},
	}
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestControllerServer_CreateVolume_FromSnapshot_WrongNode(t *testing.T) {
	f := newFixture(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]},{"node":"n2","paths":["/data"]}]}`)
	f.prov.SnapshotTracker().Put(lpp.SnapshotInfo{
		SnapshotID: "pv-src/snap", SourceVolumeID: "pv-src", Node: "n1",
		SnapshotPath: "/data/.snapshots/snap", ReadyToUse: true,
	})
	// Pod scheduled to n2, but the snapshot is on n1.
	req := validCreateReq("pv-restored", 1<<30, "n2")
	req.VolumeContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "pv-src/snap"}},
	}
	_, err := f.cs.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
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
