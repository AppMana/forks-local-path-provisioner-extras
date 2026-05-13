package csi

import (
	"context"
	"errors"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mountutils "k8s.io/mount-utils"
)

// NodeServer is the kubelet-side half of the driver. It does not need a kube
// client; everything it needs is in the request and the local filesystem.
type NodeServer struct {
	csi.UnimplementedNodeServer

	nodeID string

	// Mounter override is used by csi-sanity (mount.NewFakeMounter so the
	// bind-mount specs run without CAP_SYS_ADMIN). In production runs it is
	// nil and the platform-specific bindMount/unmountQuiet helpers are used
	// — the Linux path is a direct syscall.Mount(MS_BIND), avoiding the
	// /bin/mount shell-out that mount-utils does (the distroless runtime
	// image ships no userspace mount binary).
	fakeMounter mountutils.Interface
}

func NewNodeServer(nodeID string) *NodeServer {
	return &NodeServer{nodeID: nodeID}
}

// SetMounter swaps in a mount.Interface (csi-sanity uses
// mount.NewFakeMounter() so bind-mount specs don't need CAP_SYS_ADMIN).
// Production code path uses the platform-specific helpers in
// mounter_linux.go / mounter_other.go.
func (ns *NodeServer) SetMounter(m mountutils.Interface) {
	ns.fakeMounter = m
}

func (ns *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if ns.nodeID == "" {
		return nil, status.Error(codes.FailedPrecondition, "node ID not configured (set NODE_ID env)")
	}
	return &csi.NodeGetInfoResponse{
		NodeId: ns.nodeID,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{TopologyKeyNode: ns.nodeID},
		},
	}, nil
}

func (ns *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		// EXPAND_VOLUME added in M3.
	}
	out := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: c}},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: out}, nil
}

// NodeStageVolume / NodeUnstageVolume — we do not advertise STAGE_UNSTAGE,
// so these are not called. Stub for spec compliance.
func (ns *NodeServer) NodeStageVolume(_ context.Context, _ *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnstageVolume(_ context.Context, _ *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind-mounts the source host path (from VolumeContext)
// onto the kubelet-supplied TargetPath. Idempotent: if TargetPath is already
// a mount point onto the right source, returns success.
func (ns *NodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId missing")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "TargetPath missing")
	}
	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability missing")
	}
	source := req.VolumeContext[VolumeContextPath]
	if source == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"VolumeContext missing %q — was the volume created by this driver?", VolumeContextPath)
	}

	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "MkdirAll(%s): %v", req.TargetPath, err)
	}

	notMnt, err := ns.isLikelyNotMountPoint(req.TargetPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "IsLikelyNotMountPoint(%s): %v", req.TargetPath, err)
	}
	if !notMnt {
		logrus.Infof("NodePublishVolume %s: %s already mounted, idempotent OK", req.VolumeId, req.TargetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	logrus.Infof("NodePublishVolume %s: bind-mount %s -> %s (readonly=%v)", req.VolumeId, source, req.TargetPath, req.Readonly)
	if err := ns.bindMount(source, req.TargetPath, req.Readonly); err != nil {
		_ = os.Remove(req.TargetPath)
		return nil, status.Errorf(codes.Internal, "bind mount %s -> %s: %v", source, req.TargetPath, err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the TargetPath and removes the empty mount-point.
// Idempotent: succeeds if the path is already gone or unmounted.
func (ns *NodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId missing")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "TargetPath missing")
	}

	if _, err := os.Stat(req.TargetPath); errors.Is(err, os.ErrNotExist) {
		logrus.Infof("NodeUnpublishVolume %s: %s already gone", req.VolumeId, req.TargetPath)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	notMnt, err := ns.isLikelyNotMountPoint(req.TargetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "IsLikelyNotMountPoint(%s): %v", req.TargetPath, err)
	}
	if !notMnt {
		if err := ns.unmount(req.TargetPath); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", req.TargetPath, err)
		}
	}
	if err := os.Remove(req.TargetPath); err != nil && !os.IsNotExist(err) {
		logrus.Warnf("NodeUnpublishVolume %s: remove(%s): %v", req.VolumeId, req.TargetPath, err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// bindMount uses the fake mounter (csi-sanity) when set, otherwise the
// platform-specific helper (syscall.Mount on Linux, mount-utils elsewhere).
func (ns *NodeServer) bindMount(src, target string, readOnly bool) error {
	if ns.fakeMounter != nil {
		opts := []string{"bind"}
		if readOnly {
			opts = append(opts, "ro")
		}
		return ns.fakeMounter.Mount(src, target, "", opts)
	}
	return bindMount(src, target, readOnly)
}

func (ns *NodeServer) unmount(target string) error {
	if ns.fakeMounter != nil {
		return ns.fakeMounter.Unmount(target)
	}
	return unmountQuiet(target)
}

func (ns *NodeServer) isLikelyNotMountPoint(target string) (bool, error) {
	if ns.fakeMounter != nil {
		return ns.fakeMounter.IsLikelyNotMountPoint(target)
	}
	return isMountPoint(target)
}

// NodeGetVolumeStats reports capacity / usage at VolumePath. Used by metrics
// pipelines (kubelet exposes via /metrics/cadvisor).
func (ns *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId missing")
	}
	if req.VolumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumePath missing")
	}
	if _, err := os.Stat(req.VolumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "VolumePath %s does not exist", req.VolumePath)
		}
		return nil, status.Errorf(codes.Internal, "stat %s: %v", req.VolumePath, err)
	}
	available, capacity, used, inodesFree, inodes, inodesUsed, err := statfs(req.VolumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "statfs(%s): %v", req.VolumePath, err)
	}
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{Available: available, Total: capacity, Used: used, Unit: csi.VolumeUsage_BYTES},
			{Available: inodesFree, Total: inodes, Used: inodesUsed, Unit: csi.VolumeUsage_INODES},
		},
	}, nil
}

// NodeExpandVolume is wired in M3.
func (ns *NodeServer) NodeExpandVolume(_ context.Context, _ *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume not yet implemented (M3)")
}
