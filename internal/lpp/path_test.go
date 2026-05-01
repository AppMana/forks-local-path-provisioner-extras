package lpp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderPathPattern_Default(t *testing.T) {
	got, err := RenderPathPattern("{{.PVC.Namespace}}/{{.PVC.Name}}/data", "pv-1", "ns", "data-pvc", false)
	require.NoError(t, err)
	assert.Equal(t, "ns/data-pvc/data", got)
}

func TestRenderPathPattern_RejectsTraversal(t *testing.T) {
	got, err := RenderPathPattern("../escape", "pv-1", "ns", "data-pvc", false)
	if err == nil {
		t.Fatalf("expected error for traversal pattern, got %q", got)
	}
}

func TestRenderPathPattern_RejectsMissingPrefix(t *testing.T) {
	_, err := RenderPathPattern("just/data", "pv-1", "ns", "data-pvc", false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must start with")
}

func TestRenderPathPattern_AllowUnsafe(t *testing.T) {
	got, err := RenderPathPattern("anywhere/{{.PVName}}", "pv-1", "ns", "data-pvc", true)
	require.NoError(t, err)
	assert.Equal(t, "anywhere/pv-1", got)
}

func TestRenderPathPattern_BadTemplate(t *testing.T) {
	_, err := RenderPathPattern("{{.UnknownField}/x", "pv-1", "ns", "data-pvc", true)
	assert.Error(t, err)
}

func TestRenderPathPattern_PVNameSubstitution(t *testing.T) {
	got, err := RenderPathPattern("{{.PVC.Namespace}}/{{.PVC.Name}}/{{.PVName}}", "pv-1", "ns", "data-pvc", false)
	require.NoError(t, err)
	assert.Equal(t, "ns/data-pvc/pv-1", got)
}
