package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeServer_NodeGetInfo(t *testing.T) {
	ns := NewNodeServer("node-7")
	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "node-7", resp.NodeId)
	require.NotNil(t, resp.AccessibleTopology)
	assert.Equal(t, "node-7", resp.AccessibleTopology.Segments[TopologyKeyNode])
}

func TestNodeServer_NodeGetInfo_MissingID(t *testing.T) {
	ns := NewNodeServer("")
	_, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestNodeServer_NodeGetCapabilities(t *testing.T) {
	ns := NewNodeServer("n")
	resp, err := ns.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	require.NoError(t, err)
	caps := map[csi.NodeServiceCapability_RPC_Type]bool{}
	for _, c := range resp.Capabilities {
		caps[c.GetRpc().Type] = true
	}
	assert.True(t, caps[csi.NodeServiceCapability_RPC_GET_VOLUME_STATS])
	// EXPAND_VOLUME comes in M3.
	assert.False(t, caps[csi.NodeServiceCapability_RPC_EXPAND_VOLUME])
}

func TestNodeServer_NodeStageUnstage_Noop(t *testing.T) {
	ns := NewNodeServer("n")
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{})
	require.NoError(t, err)
	_, err = ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{})
	require.NoError(t, err)
}

func TestNodeServer_NodePublishVolume_ValidationErrors(t *testing.T) {
	ns := NewNodeServer("n")

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "v"})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:   "v",
		TargetPath: "/tmp/whatever",
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "v",
		TargetPath:       "/tmp/whatever",
		VolumeCapability: &csi.VolumeCapability{},
		VolumeContext:    map[string]string{},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), VolumeContextPath)
}

func TestNodeServer_NodeUnpublishVolume_ValidationErrors(t *testing.T) {
	ns := NewNodeServer("n")
	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{})
	require.Error(t, err)
	_, err = ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "v"})
	require.Error(t, err)
}

func TestNodeServer_NodeUnpublishVolume_MissingPath_Idempotent(t *testing.T) {
	ns := NewNodeServer("n")
	dir := t.TempDir()
	target := filepath.Join(dir, "missing")
	resp, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "v",
		TargetPath: target,
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestNodeServer_NodeUnpublishVolume_NotMounted_RemovesDir(t *testing.T) {
	// If TargetPath exists but is not a mount point, NodeUnpublishVolume just
	// removes the directory.
	ns := NewNodeServer("n")
	dir := t.TempDir()
	target := filepath.Join(dir, "tp")
	require.NoError(t, os.MkdirAll(target, 0o750))

	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "v",
		TargetPath: target,
	})
	require.NoError(t, err)
	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "target dir should be removed; got %v", statErr)
}

func TestNodeServer_NodeGetVolumeStats_MissingPath(t *testing.T) {
	ns := NewNodeServer("n")
	_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "v",
		VolumePath: "/nonexistent/path/should/not/exist",
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestNodeServer_NodeGetVolumeStats_RealDir(t *testing.T) {
	ns := NewNodeServer("n")
	dir := t.TempDir()
	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "v",
		VolumePath: dir,
	})
	require.NoError(t, err)
	require.Len(t, resp.Usage, 2)
	// Bytes is always populated. Inodes is zero on tmpfs (which t.TempDir
	// may use under containers); don't assert non-zero on it.
	bytes := resp.Usage[0]
	assert.Equal(t, csi.VolumeUsage_BYTES, bytes.Unit)
	assert.Greater(t, bytes.Total, int64(0))
	assert.Equal(t, csi.VolumeUsage_INODES, resp.Usage[1].Unit)
}

func TestNodeServer_NodeExpandVolume_Unimplemented(t *testing.T) {
	ns := NewNodeServer("n")
	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestParseEndpoint(t *testing.T) {
	scheme, addr, err := parseEndpoint("unix:///tmp/csi.sock")
	require.NoError(t, err)
	assert.Equal(t, "unix", scheme)
	assert.Equal(t, "/tmp/csi.sock", addr)

	scheme, addr, err = parseEndpoint("tcp://127.0.0.1:9000")
	require.NoError(t, err)
	assert.Equal(t, "tcp", scheme)
	assert.Equal(t, "127.0.0.1:9000", addr)

	_, _, err = parseEndpoint("https://x")
	assert.Error(t, err)
}
