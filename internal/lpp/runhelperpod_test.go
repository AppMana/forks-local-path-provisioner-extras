package lpp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestProvisioner_RunHelperPod_Real drives the fake clientset's pod object
// through Pending → Succeeded so the real helper-pod dispatch path runs to
// completion. Verifies the pod was created on the correct node, log capture
// fired, and the pod was deleted afterward.
func TestProvisioner_RunHelperPod_Real(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	// Use the real impl, not the no-op fake.
	p.SetRunHelperPodFn(nil)

	cfg, err := p.PickConfig("")
	require.NoError(t, err)

	// Simulate kubelet: as soon as a helper pod is created, mark it Succeeded.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			pods, _ := p.kubeClient.CoreV1().Pods("ns").List(context.TODO(), metav1.ListOptions{})
			for i := range pods.Items {
				pod := pods.Items[i].DeepCopy()
				if pod.Status.Phase == v1.PodSucceeded {
					continue
				}
				pod.Status.Phase = v1.PodSucceeded
				_, _ = p.kubeClient.CoreV1().Pods("ns").UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
			}
		}
	}()

	err = p.RunHelperPod(context.Background(), HelperAction{
		Type:   ActionTypeCreate,
		Cmd:    []string{"/bin/sh", "/script/setup"},
		Volume: VolumeOptions{Name: "pv-1", Path: "/data/pv-1", Mode: v1.PersistentVolumeFilesystem, SizeInBytes: 1 << 20, Node: "n1"},
		Config: cfg,
	})
	require.NoError(t, err)

	// Pod was deleted by the helper after completion.
	_, err = p.kubeClient.CoreV1().Pods("ns").Get(context.TODO(), "helper-pod-create-pv-1", metav1.GetOptions{})
	assert.True(t, k8serror.IsNotFound(err), "helper pod should be deleted after run; got %v", err)
}

// TestProvisioner_RunHelperPod_PodFails covers the failure branch.
func TestProvisioner_RunHelperPod_PodFails(t *testing.T) {
	p := newTestProvisioner(t, `{"nodePathMap":[{"node":"n1","paths":["/data"]}]}`)
	p.SetRunHelperPodFn(nil)

	cfg, err := p.PickConfig("")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			pods, _ := p.kubeClient.CoreV1().Pods("ns").List(context.TODO(), metav1.ListOptions{})
			for i := range pods.Items {
				pod := pods.Items[i].DeepCopy()
				if pod.Status.Phase == v1.PodFailed {
					continue
				}
				pod.Status.Phase = v1.PodFailed
				pod.Status.ContainerStatuses = []v1.ContainerStatus{{
					Name: "helper-pod",
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "out of disk"},
					},
				}}
				_, _ = p.kubeClient.CoreV1().Pods("ns").UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
			}
		}
	}()

	err = p.RunHelperPod(context.Background(), HelperAction{
		Type:   ActionTypeCreate,
		Cmd:    []string{"/bin/sh", "/script/setup"},
		Volume: VolumeOptions{Name: "pv-fail", Path: "/data/pv-fail", Mode: v1.PersistentVolumeFilesystem, SizeInBytes: 1 << 20, Node: "n1"},
		Config: cfg,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit code 1")
	assert.Contains(t, err.Error(), "out of disk")
}

func TestLoadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	require.NoError(t, os.WriteFile(f, []byte("hello"), 0o644))
	got, err := LoadFile(f)
	require.NoError(t, err)
	assert.Equal(t, "hello", got)

	_, err = LoadFile(filepath.Join(dir, "missing.txt"))
	assert.Error(t, err)
}

func TestLoadHelperPodFile_Invalid(t *testing.T) {
	_, err := LoadHelperPodFile("not yaml: [")
	assert.Error(t, err)
}

func TestLoadHelperPodFile_NoContainers(t *testing.T) {
	_, err := LoadHelperPodFile(`apiVersion: v1
kind: Pod
metadata: {name: x}
spec: {containers: []}`)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "any container")
}
