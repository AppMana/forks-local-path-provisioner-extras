package csi

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/local-path-provisioner/internal/lpp"
)

// ControllerServer implements csi.ControllerServer on top of the existing
// LocalPathProvisioner core (helper-pod dispatch, configmap parsing,
// CapacityTracker).
type ControllerServer struct {
	csi.UnimplementedControllerServer

	provisioner *lpp.Provisioner
}

func NewControllerServer(p *lpp.Provisioner) *ControllerServer {
	return &ControllerServer{provisioner: p}
}

func (cs *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	}
	out := make([]*csi.ControllerServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: out}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId missing")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapabilities missing")
	}
	for _, c := range req.VolumeCapabilities {
		mode := c.GetAccessMode().GetMode()
		if mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
			mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER &&
			mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf("unsupported access mode %v", mode),
			}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name missing")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities missing")
	}

	requestedBytes := req.GetCapacityRange().GetRequiredBytes()
	if requestedBytes <= 0 {
		requestedBytes = 1 << 30 // default 1GiB
	}

	pvName := req.Name
	pvcName := req.Parameters[ParamPVCName]
	pvcNamespace := req.Parameters[ParamPVCNamespace]
	if pvcName == "" || pvcNamespace == "" {
		return nil, status.Error(codes.InvalidArgument,
			"PVC metadata missing — csi-provisioner must run with --extra-create-metadata=true")
	}

	cfgName := req.Parameters["storageClassConfig"]
	cfg, err := cs.provisioner.PickConfig(cfgName)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "pickConfig: %v", err)
	}
	sharedFS, err := cs.provisioner.IsSharedFilesystem(cfg)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "isSharedFilesystem: %v", err)
	}

	if err := cs.validateCapabilities(req.VolumeCapabilities, sharedFS); err != nil {
		return nil, err
	}

	// Validate min/max from config + StorageClass parameters (params override).
	storage := *resource.NewQuantity(requestedBytes, resource.BinarySI)
	minSize := cfg.MinSize
	maxSize := cfg.MaxSize
	if v, ok := req.Parameters["minSize"]; ok {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid minSize %q: %v", v, err)
		}
		minSize = &q
	}
	if v, ok := req.Parameters["maxSize"]; ok {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid maxSize %q: %v", v, err)
		}
		maxSize = &q
	}
	if minSize != nil && storage.Cmp(*minSize) < 0 {
		return nil, status.Errorf(codes.OutOfRange, "PVC requests %v which is below minimum size %v", storage.String(), minSize.String())
	}
	if maxSize != nil && storage.Cmp(*maxSize) > 0 {
		return nil, status.Errorf(codes.OutOfRange, "PVC requests %v which exceeds maximum size %v", storage.String(), maxSize.String())
	}

	// Pick node from preferred topology unless shared FS.
	var nodeName string
	if !sharedFS {
		nodeName = pickPreferredNode(req.AccessibilityRequirements)
		if nodeName == "" {
			return nil, status.Error(codes.InvalidArgument,
				"no preferred topology with kubernetes.io/hostname provided — "+
					"set the StorageClass volumeBindingMode to WaitForFirstConsumer")
		}
	}

	// Pick path on node, atomically allocating capacity.
	requestedPath := req.Parameters["nodePath"]
	basePath, err := cs.provisioner.GetPathOnNode(nodeName, requestedPath, cfg, requestedBytes)
	if err != nil {
		return nil, status.Errorf(codes.ResourceExhausted, "%v", err)
	}
	allocated := true
	defer func() {
		if allocated {
			cs.provisioner.ReleaseCapacity(nodeName, basePath, requestedBytes)
		}
	}()

	// Compute folder name (optionally from pathPattern).
	folderName, err := cs.computeFolderName(req, pvName, pvcNamespace, pvcName)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(basePath, folderName)

	// Quota / volume mode propagation to helper script env.
	quotaType := req.Parameters["quotaEnforcement"]
	volumeMode := v1.PersistentVolumeFilesystem
	for _, c := range req.VolumeCapabilities {
		if c.GetBlock() != nil {
			volumeMode = v1.PersistentVolumeBlock
			break
		}
	}

	// Run setup helper pod.
	setupCmd := cs.provisioner.SetupCommand()
	if err := cs.provisioner.RunHelperPod(ctx, lpp.HelperAction{
		Type: lpp.ActionTypeCreate,
		Cmd:  setupCmd,
		Volume: lpp.VolumeOptions{
			Name:        pvName,
			Path:        path,
			Mode:        volumeMode,
			SizeInBytes: requestedBytes,
			Node:        nodeName,
			QuotaType:   quotaType,
		},
		Config: cfg,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "helper pod failed: %v", err)
	}

	allocated = false
	logrus.Infof("CreateVolume %s on %s:%s (%d bytes, quota=%q)", pvName, nodeName, path, requestedBytes, quotaType)

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      pvName,
			CapacityBytes: requestedBytes,
			VolumeContext: map[string]string{
				VolumeContextNode:      nodeName,
				VolumeContextPath:      path,
				VolumeContextQuotaType: quotaType,
			},
		},
	}
	if !sharedFS {
		resp.Volume.AccessibleTopology = []*csi.Topology{
			{Segments: map[string]string{TopologyKeyNode: nodeName}},
		}
	}
	return resp, nil
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId missing")
	}

	// Look up the PV to recover node + path. If the PV was already deleted,
	// DeleteVolume is idempotent — return success.
	pv, err := cs.provisioner.KubeClient().CoreV1().PersistentVolumes().Get(ctx, req.VolumeId, metav1.GetOptions{})
	if err != nil {
		if k8serror.IsNotFound(err) {
			logrus.Infof("DeleteVolume %s: PV not found, treating as already deleted", req.VolumeId)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "PV lookup failed: %v", err)
	}

	src := pv.Spec.PersistentVolumeSource
	if src.CSI == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "PV %s is not a CSI volume", req.VolumeId)
	}
	attrs := src.CSI.VolumeAttributes
	nodeName := attrs[VolumeContextNode]
	path := attrs[VolumeContextPath]
	quotaType := attrs[VolumeContextQuotaType]

	if path == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "PV %s has no path attribute", req.VolumeId)
	}

	// Skip destroy if reclaim is Retain.
	if pv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimRetain {
		logrus.Infof("DeleteVolume %s: reclaim policy Retain, leaving %s on %s", req.VolumeId, path, nodeName)
		return &csi.DeleteVolumeResponse{}, nil
	}

	// If node is gone, just release tracker capacity.
	if nodeName != "" {
		if _, err := cs.provisioner.KubeClient().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{}); err != nil && k8serror.IsNotFound(err) {
			logrus.Infof("DeleteVolume %s: node %s gone, skipping helper pod", req.VolumeId, nodeName)
			storage := pv.Spec.Capacity[v1.ResourceStorage]
			cs.provisioner.ReleaseCapacity(nodeName, filepath.Dir(path), storage.Value())
			return &csi.DeleteVolumeResponse{}, nil
		}
	}

	cfg, err := cs.provisioner.PickConfig(pv.Spec.StorageClassName)
	if err != nil {
		// Fall back to default if SC name doesn't resolve.
		cfg, err = cs.provisioner.PickConfig("")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "pickConfig: %v", err)
		}
	}

	teardownCmd := cs.provisioner.TeardownCommand()
	storage := pv.Spec.Capacity[v1.ResourceStorage]
	mode := v1.PersistentVolumeFilesystem
	if pv.Spec.VolumeMode != nil {
		mode = *pv.Spec.VolumeMode
	}
	if err := cs.provisioner.RunHelperPod(ctx, lpp.HelperAction{
		Type: lpp.ActionTypeDelete,
		Cmd:  teardownCmd,
		Volume: lpp.VolumeOptions{
			Name:        pv.Name,
			Path:        path,
			Mode:        mode,
			SizeInBytes: storage.Value(),
			Node:        nodeName,
			QuotaType:   quotaType,
		},
		Config: cfg,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "helper pod failed: %v", err)
	}
	cs.provisioner.ReleaseCapacity(nodeName, filepath.Dir(path), storage.Value())
	logrus.Infof("DeleteVolume %s: removed %s on %s", req.VolumeId, path, nodeName)
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume / ControllerUnpublishVolume are no-ops for local volumes
// (CSIDriver.spec.attachRequired=false), but the spec still requires the RPC handlers.
func (cs *ControllerServer) ControllerPublishVolume(_ context.Context, _ *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerUnpublishVolume(_ context.Context, _ *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ControllerExpandVolume is wired in M3.
func (cs *ControllerServer) ControllerExpandVolume(_ context.Context, _ *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerExpandVolume not yet implemented (M3)")
}

// GetCapacity is wired in M2.
func (cs *ControllerServer) GetCapacity(_ context.Context, _ *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetCapacity not yet implemented (M2)")
}

// CreateSnapshot / DeleteSnapshot / ListSnapshots are wired in M7.
func (cs *ControllerServer) CreateSnapshot(_ context.Context, _ *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateSnapshot not yet implemented (M7)")
}

func (cs *ControllerServer) DeleteSnapshot(_ context.Context, _ *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DeleteSnapshot not yet implemented (M7)")
}

func (cs *ControllerServer) ListSnapshots(_ context.Context, _ *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListSnapshots not yet implemented (M7)")
}

// validateCapabilities rejects unsupported access modes early.
func (cs *ControllerServer) validateCapabilities(caps []*csi.VolumeCapability, sharedFS bool) error {
	for _, c := range caps {
		mode := c.GetAccessMode().GetMode()
		if !sharedFS &&
			mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
			mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER &&
			mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER {
			return status.Errorf(codes.InvalidArgument,
				"local-path volumes only support SINGLE_NODE_* access modes, got %v", mode)
		}
	}
	return nil
}

// computeFolderName mirrors the upstream pathPattern logic but takes plain
// pvName/pvcNamespace/pvcName instead of a sigs.k8s.io/sig-storage-lib type.
func (cs *ControllerServer) computeFolderName(req *csi.CreateVolumeRequest, pvName, pvcNamespace, pvcName string) (string, error) {
	pattern, ok := req.Parameters["pathPattern"]
	if !ok {
		return pvName + "_" + pvcNamespace + "_" + pvcName, nil
	}

	allowUnsafePath := false
	if v, ok := req.Parameters["allowUnsafePathPattern"]; ok {
		b, err := strconv.ParseBool(v)
		if err == nil {
			allowUnsafePath = b
		}
	}

	folder, err := lpp.RenderPathPattern(pattern, pvName, pvcNamespace, pvcName, allowUnsafePath)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "pathPattern: %v", err)
	}
	if !allowUnsafePath && !filepath.IsLocal(folder) {
		return "", status.Errorf(codes.InvalidArgument, "folder path contains invalid references: %s", folder)
	}
	return folder, nil
}

// pickPreferredNode walks AccessibilityRequirements.Preferred for the topology
// segment kubernetes.io/hostname; falls back to Requisite. Returns "" if none.
func pickPreferredNode(reqs *csi.TopologyRequirement) string {
	if reqs == nil {
		return ""
	}
	for _, t := range reqs.Preferred {
		if v, ok := t.Segments[TopologyKeyNode]; ok && v != "" {
			return v
		}
	}
	for _, t := range reqs.Requisite {
		if v, ok := t.Segments[TopologyKeyNode]; ok && v != "" {
			return v
		}
	}
	return ""
}
