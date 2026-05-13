package lpp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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

// newTestProvisioner constructs a Provisioner backed by a fake clientset and a
// no-op helper-pod runner. The supplied configJSON is parsed as the in-memory
// configFile (LoadConfigFile accepts an inline JSON string).
func newTestProvisioner(t *testing.T, configJSON string, prepop ...runtime.Object) *Provisioner {
	t.Helper()
	kc := fake.NewSimpleClientset(prepop...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Inline JSON skips the watch goroutine (see NewProvisioner). No need
	// to mutate the package-level ConfigFileCheckInterval.
	p, err := NewProvisioner(ctx, kc, configJSON, "ns", "img", "lpp-cm", "lpp-sa", helperPodYaml)
	require.NoError(t, err)
	p.SetRunHelperPodFn(func(_ context.Context, _ HelperAction) error { return nil })
	return p
}

func TestProvisioner_PickConfig_Default(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n1","paths":["/a"]}]}`)
	c, err := p.PickConfig("anything")
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Contains(t, c.NodePathMap, "n1")
}

func TestProvisioner_PickConfig_NamedSC(t *testing.T) {
	cfg := `{
      "storageClassConfigs": {
        "fast": {"nodePathMap":[{"node":"n1","paths":["/fast"]}]},
        "slow": {"nodePathMap":[{"node":"n1","paths":["/slow"]}]}
      }
    }`
	p := newTestProvisioner(t, cfg)
	fast, err := p.PickConfig("fast")
	require.NoError(t, err)
	assert.Contains(t, fast.NodePathMap["n1"].Paths, "/fast")

	slow, err := p.PickConfig("slow")
	require.NoError(t, err)
	assert.Contains(t, slow.NodePathMap["n1"].Paths, "/slow")

	_, err = p.PickConfig("missing")
	assert.Error(t, err)
}

func TestProvisioner_IsSharedFilesystem(t *testing.T) {
	p := newTestProvisioner(t, `{"sharedFileSystemPath":"/shared"}`)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	shared, err := p.IsSharedFilesystem(c)
	require.NoError(t, err)
	assert.True(t, shared)

	p2 := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/a"]}]}`)
	c2, err := p2.PickConfig("")
	require.NoError(t, err)
	shared, err = p2.IsSharedFilesystem(c2)
	require.NoError(t, err)
	assert.False(t, shared)
}

func TestProvisioner_GetPathOnNode_DefaultUnlistedNode(t *testing.T) {
	cfg := `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/opt/lpp"]}]}`
	p := newTestProvisioner(t, cfg)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	path, err := p.GetPathOnNode("anynode", "", c, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, "/opt/lpp", path)
}

func TestProvisioner_GetPathOnNode_RequestedPath(t *testing.T) {
	cfg := `{"nodePathMap":[{"node":"n","paths":["/a","/b"]}]}`
	p := newTestProvisioner(t, cfg)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	got, err := p.GetPathOnNode("n", "/b", c, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, "/b", got)

	_, err = p.GetPathOnNode("n", "/missing", c, 1<<20)
	assert.Error(t, err)
}

func TestProvisioner_GetPathOnNode_CapacityExhausted(t *testing.T) {
	cfg := `{"nodePathMap":[{"node":"n","paths":[{"path":"/a","maxCapacity":"1Ki"}]}]}`
	p := newTestProvisioner(t, cfg)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	_, err = p.GetPathOnNode("n", "/a", c, 2048) // 2 KiB > 1 KiB limit
	assert.Error(t, err)

	got, err := p.GetPathOnNode("n", "/a", c, 512)
	require.NoError(t, err)
	assert.Equal(t, "/a", got)
	// Now the path is full from the controller's perspective.
	_, err = p.GetPathOnNode("n", "/a", c, 600)
	assert.Error(t, err)
}

func TestProvisioner_GetPathOnNode_RandomPick(t *testing.T) {
	cfg := `{"nodePathMap":[{"node":"n","paths":["/a","/b","/c"]}]}`
	p := newTestProvisioner(t, cfg)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		path, err := p.GetPathOnNode("n", "", c, 1<<20)
		require.NoError(t, err)
		assert.Contains(t, []string{"/a", "/b", "/c"}, path)
	}
}

func TestProvisioner_GetPathOnNode_UnknownNodeNoDefault(t *testing.T) {
	cfg := `{"nodePathMap":[{"node":"n1","paths":["/a"]}]}`
	p := newTestProvisioner(t, cfg)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	_, err = p.GetPathOnNode("unknown", "", c, 1<<20)
	assert.Error(t, err)
}

func TestProvisioner_ReleaseCapacity(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":[{"path":"/a","maxCapacity":"1Ki"}]}]}`)
	c, err := p.PickConfig("")
	require.NoError(t, err)
	_, err = p.GetPathOnNode("n", "/a", c, 800)
	require.NoError(t, err)
	assert.Equal(t, int64(800), p.CapacityTracker().GetAllocated("n", "/a"))

	p.ReleaseCapacity("n", "/a", 800)
	assert.Equal(t, int64(0), p.CapacityTracker().GetAllocated("n", "/a"))
}

func TestProvisioner_InitCapacityTracker_FromExistingPVs(t *testing.T) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-existing"},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver: "local-path.appmana.io",
					VolumeAttributes: map[string]string{
						"node": "n",
						"path": "/data/pv-existing_default_existing",
					},
				},
			},
		},
	}
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/data"]}]}`, pv)
	got := p.CapacityTracker().GetAllocated("n", "/data")
	assert.Equal(t, int64(1<<30), got, "1Gi from existing CSI PV should be loaded into tracker")
}

func TestProvisioner_InitCapacityTracker_FromLegacyHostPathPV(t *testing.T) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pv-legacy",
			Annotations: map[string]string{NodeNameAnnotationKey: "n"},
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{v1.ResourceStorage: resource.MustParse("2Gi")},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{Path: "/data/legacy/data"},
			},
		},
	}
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/data/legacy"]}]}`, pv)
	got := p.CapacityTracker().GetAllocated("n", "/data/legacy")
	assert.Equal(t, int64(2)<<30, got)
}

func TestProvisioner_DefaultCommands(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/a"]}]}`)
	// Defaults now point at the helper image's /usr/local/sbin scripts;
	// the legacy /script/* configmap-mount fallback path is exercised
	// only when a script is missing from config AND the helper pod
	// template requests a configmap mount.
	assert.Equal(t, []string{"/usr/local/sbin/setup.sh"}, p.SetupCommand())
	assert.Equal(t, []string{"/usr/local/sbin/teardown.sh"}, p.TeardownCommand())
	assert.Equal(t, []string{"/usr/local/sbin/resize.sh"}, p.ResizeCommand())
}

func TestProvisioner_OverrideCommands(t *testing.T) {
	p := newTestProvisioner(t, `{
      "setupCommand":"/usr/local/bin/setup",
      "teardownCommand":"/usr/local/bin/teardown",
      "resizeCommand":"/usr/local/bin/resize",
      "nodePathMap":[{"node":"n","paths":["/a"]}]
    }`)
	assert.Equal(t, []string{"/usr/local/bin/setup"}, p.SetupCommand())
	assert.Equal(t, []string{"/usr/local/bin/teardown"}, p.TeardownCommand())
	assert.Equal(t, []string{"/usr/local/bin/resize"}, p.ResizeCommand())
}

