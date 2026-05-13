package lpp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPathIsAbsForOS(t *testing.T) {
	cases := []struct {
		path, os string
		want     bool
	}{
		{"/opt/lpp", OSLinux, true},
		{"opt/lpp", OSLinux, false},
		{"C:\\data\\x", OSWindows, true},
		{"C:/data/x", OSWindows, true},
		{"data\\x", OSWindows, false},
		{"/opt/x", OSWindows, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, pathIsAbsForOS(c.path, c.os), "path=%s os=%s", c.path, c.os)
	}
}

func TestSplitPathForOS_Windows(t *testing.T) {
	dir, file := splitPathForOS(`C:\data\pvc-abc_default_data`, OSWindows)
	assert.Equal(t, `C:\data\`, dir)
	assert.Equal(t, "pvc-abc_default_data", file)
}

func TestJoinPathForOS_Windows(t *testing.T) {
	got := joinPathForOS(OSWindows, `C:\data\`, "pvc-abc")
	assert.Equal(t, `C:\data\pvc-abc`, got)
}

func TestCleanPathForOS_Windows(t *testing.T) {
	assert.Equal(t, `C:\data\x`, cleanPathForOS(`C:/data//x/`, OSWindows))
	assert.Equal(t, `C:\`, cleanPathForOS(`C:\`, OSWindows))
}

func TestResolveTargetOS(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lin", Labels: map[string]string{"kubernetes.io/os": "linux"}}},
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "win", Labels: map[string]string{"kubernetes.io/os": "windows"}}},
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
	)
	p := &Provisioner{kubeClient: kc}
	assert.Equal(t, OSLinux, p.resolveTargetOS(context.Background(), "lin"))
	assert.Equal(t, OSWindows, p.resolveTargetOS(context.Background(), "win"))
	assert.Equal(t, OSLinux, p.resolveTargetOS(context.Background(), "bare"))
	assert.Equal(t, OSLinux, p.resolveTargetOS(context.Background(), "ghost"))
	assert.Equal(t, OSLinux, p.resolveTargetOS(context.Background(), ""))
}

func TestCommandFor_LinuxDefaults(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/a"]}]}`)
	assert.Equal(t, []string{"/usr/local/sbin/setup.sh"}, p.commandFor(ActionTypeCreate, OSLinux, nil))
	assert.Equal(t, []string{"/usr/local/sbin/teardown.sh"}, p.commandFor(ActionTypeDelete, OSLinux, nil))
	assert.Equal(t, []string{"/usr/local/sbin/snapshot.sh"}, p.commandFor(ActionTypeSnapshot, OSLinux, nil))
	assert.Equal(t, []string{"/usr/local/sbin/snapshot.sh"}, p.commandFor(ActionTypeDeleteSnapshot, OSLinux, nil))
}

func TestCommandFor_WindowsDefaults(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/a"]}]}`)
	got := p.commandFor(ActionTypeCreate, OSWindows, nil)
	assert.Equal(t, []string{"powershell", "-NoProfile", "-File", `C:\opt\local-path-csi-scripts\setup.ps1`}, got)
}

func TestCommandFor_WindowsOverride_String(t *testing.T) {
	cfg := `{
      "nodePathMap":[{"node":"n","paths":["/a"]}],
      "windows":{"setupCommand":"powershell -File X:\\custom.ps1"}
    }`
	p := newTestProvisioner(t, cfg)
	got := p.commandFor(ActionTypeCreate, OSWindows, nil)
	assert.Equal(t, []string{"powershell", "-File", `X:\custom.ps1`}, got)
}

func TestCommandFor_WindowsOverride_Array(t *testing.T) {
	cfg := `{
      "nodePathMap":[{"node":"n","paths":["/a"]}],
      "windows":{"snapshotCommand":["pwsh","-File","C:\\Program Files\\snap.ps1"]}
    }`
	p := newTestProvisioner(t, cfg)
	got := p.commandFor(ActionTypeSnapshot, OSWindows, nil)
	assert.Equal(t, []string{"pwsh", "-File", `C:\Program Files\snap.ps1`}, got)
}

// Per-StorageClass overrides take precedence over the global / per-OS
// overrides on the top-level config. Linux path.
func TestCommandFor_StorageClassOverride_Linux(t *testing.T) {
	cfg := `{
      "storageClassConfigs": {
        "fast": {
          "nodePathMap": [{"node":"n","paths":["/fast"]}],
          "setupCommand": "/opt/fast-setup.sh",
          "snapshotCommand": "/opt/fast-snapshot.sh"
        },
        "slow": {
          "nodePathMap": [{"node":"n","paths":["/slow"]}]
        }
      },
      "setupCommand": "/opt/global-setup.sh"
    }`
	p := newTestProvisioner(t, cfg)
	fast := p.config.StorageClassConfigs["fast"]
	slow := p.config.StorageClassConfigs["slow"]

	// fast: setup + snapshot are per-SC. teardown falls through to global default.
	assert.Equal(t, []string{"/opt/fast-setup.sh"}, p.commandFor(ActionTypeCreate, OSLinux, &fast))
	assert.Equal(t, []string{"/opt/fast-snapshot.sh"}, p.commandFor(ActionTypeSnapshot, OSLinux, &fast))
	assert.Equal(t, []string{"/usr/local/sbin/teardown.sh"}, p.commandFor(ActionTypeDelete, OSLinux, &fast))

	// slow has no per-SC commands: setup falls back to the global override, the rest to per-OS defaults.
	assert.Equal(t, []string{"/opt/global-setup.sh"}, p.commandFor(ActionTypeCreate, OSLinux, &slow))
	assert.Equal(t, []string{"/usr/local/sbin/snapshot.sh"}, p.commandFor(ActionTypeSnapshot, OSLinux, &slow))
}

// Per-StorageClass override applies on Windows too, including the
// string-or-array shape on the Windows sub-block.
func TestCommandFor_StorageClassOverride_Windows(t *testing.T) {
	cfg := `{
      "storageClassConfigs": {
        "winfast": {
          "nodePathMap": [{"node":"n","paths":["/fast"]}],
          "windows": {
            "setupCommand": ["pwsh","-File","C:\\opt\\fast.ps1"],
            "snapshotCommand": "powershell -File C:\\opt\\snap.ps1"
          }
        }
      }
    }`
	p := newTestProvisioner(t, cfg)
	winfast := p.config.StorageClassConfigs["winfast"]

	assert.Equal(t,
		[]string{"pwsh", "-File", `C:\opt\fast.ps1`},
		p.commandFor(ActionTypeCreate, OSWindows, &winfast))
	assert.Equal(t,
		[]string{"powershell", "-File", `C:\opt\snap.ps1`},
		p.commandFor(ActionTypeSnapshot, OSWindows, &winfast))

	// teardown not overridden: falls back to the per-OS default.
	assert.Equal(t,
		[]string{"powershell", "-NoProfile", "-File", `C:\opt\local-path-csi-scripts\teardown.ps1`},
		p.commandFor(ActionTypeDelete, OSWindows, &winfast))
}

func TestSetWindowsHelperPodTemplate(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n","paths":["/a"]}]}`)
	assert.False(t, p.HasWindowsHelperPodTemplate())

	err := p.SetWindowsHelperPodTemplate(`
apiVersion: v1
kind: Pod
metadata: {name: helper-pod}
spec:
  containers:
    - name: helper-pod
      image: ghcr.io/appmana/local-path-helper:latest
`)
	assert.NoError(t, err)
	assert.True(t, p.HasWindowsHelperPodTemplate())

	// Clearing via empty string.
	assert.NoError(t, p.SetWindowsHelperPodTemplate(""))
	assert.False(t, p.HasWindowsHelperPodTemplate())

	// Invalid YAML must error and leave the previous value untouched.
	_ = p.SetWindowsHelperPodTemplate(`apiVersion: v1
kind: Pod
metadata: {name: x}
spec: {containers: []}`)
	assert.False(t, p.HasWindowsHelperPodTemplate())
}
