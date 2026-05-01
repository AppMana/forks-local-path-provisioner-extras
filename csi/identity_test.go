package csi

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityServer_GetPluginInfo(t *testing.T) {
	s := NewIdentityServer()
	resp, err := s.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, DriverName, resp.Name)
	assert.Equal(t, DriverVersion, resp.VendorVersion)
}

func TestIdentityServer_GetPluginCapabilities(t *testing.T) {
	s := NewIdentityServer()
	resp, err := s.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	require.NoError(t, err)

	types := map[csi.PluginCapability_Service_Type]bool{}
	for _, c := range resp.Capabilities {
		if svc := c.GetService(); svc != nil {
			types[svc.Type] = true
		}
	}
	assert.True(t, types[csi.PluginCapability_Service_CONTROLLER_SERVICE], "must advertise CONTROLLER_SERVICE")
	assert.True(t, types[csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS], "must advertise VOLUME_ACCESSIBILITY_CONSTRAINTS for topology-aware scheduling")
}

func TestIdentityServer_Probe(t *testing.T) {
	s := NewIdentityServer()
	_, err := s.Probe(context.Background(), &csi.ProbeRequest{})
	require.NoError(t, err)
}
