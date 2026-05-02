package lpp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCapacityReporter_ReportOnce_WritesAnnotation(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a")
	pathB := filepath.Join(dir, "b")
	require.NoError(t, os.MkdirAll(pathA, 0o755))
	require.NoError(t, os.MkdirAll(pathB, 0o755))

	kc := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	r := NewCapacityReporter(kc, "node-1", []string{pathA, pathB}, time.Hour)
	r.SetStatfs(func(p string) (int64, error) {
		switch p {
		case pathA:
			return 1 << 30, nil
		case pathB:
			return 5 << 30, nil
		}
		return 0, nil
	})
	require.NoError(t, r.reportOnce(context.Background()))

	got, err := kc.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	require.NoError(t, err)
	annotation := got.Annotations[FreeBytesAnnotationKey]
	assert.NotEmpty(t, annotation)

	free := FreeBytesByPath(annotation)
	require.NotNil(t, free)
	assert.Equal(t, int64(1<<30), free[pathA])
	assert.Equal(t, int64(5<<30), free[pathB])
}

func TestCapacityReporter_MissingPath_ReportsZero(t *testing.T) {
	kc := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	r := NewCapacityReporter(kc, "node-1", []string{"/nonexistent/path/x"}, time.Hour)
	require.NoError(t, r.reportOnce(context.Background()))

	got, err := kc.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	require.NoError(t, err)
	free := FreeBytesByPath(got.Annotations[FreeBytesAnnotationKey])
	require.NotNil(t, free)
	assert.Equal(t, int64(0), free["/nonexistent/path/x"])
}

func TestCapacityReporter_StatfsError_ReportsZero(t *testing.T) {
	dir := t.TempDir()
	kc := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	r := NewCapacityReporter(kc, "node-1", []string{dir}, time.Hour)
	r.SetStatfs(func(string) (int64, error) { return 0, assert.AnError })
	require.NoError(t, r.reportOnce(context.Background()))

	got, _ := kc.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	free := FreeBytesByPath(got.Annotations[FreeBytesAnnotationKey])
	assert.Equal(t, int64(0), free[dir])
}

func TestCapacityReporter_RunStopsWithContext(t *testing.T) {
	kc := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	dir := t.TempDir()
	r := NewCapacityReporter(kc, "node-1", []string{dir}, 10*time.Millisecond)
	r.SetStatfs(func(string) (int64, error) { return 100, nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(40 * time.Millisecond) // let the loop fire a few times
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reporter did not stop on context cancel")
	}
}

func TestFreeBytesByPath_InvalidJSON(t *testing.T) {
	assert.Nil(t, FreeBytesByPath(""))
	assert.Nil(t, FreeBytesByPath("not json"))
	got := FreeBytesByPath(`{"/a":123}`)
	require.NotNil(t, got)
	assert.Equal(t, int64(123), got["/a"])
}

func TestCapacityReporter_NoNodeID_NoOp(t *testing.T) {
	kc := fake.NewSimpleClientset()
	r := NewCapacityReporter(kc, "", []string{"/x"}, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()
	<-done
	// The empty-nodeID case must return immediately (no panic, no patch).
}

func TestCapacityReporter_AnnotationFormatStable(t *testing.T) {
	// Defends the JSON format the controller reads.
	dir := t.TempDir()
	kc := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}})
	r := NewCapacityReporter(kc, "n", []string{dir}, time.Hour)
	r.SetStatfs(func(string) (int64, error) { return 42, nil })
	require.NoError(t, r.reportOnce(context.Background()))

	got, _ := kc.CoreV1().Nodes().Get(context.Background(), "n", metav1.GetOptions{})
	annotation := got.Annotations[FreeBytesAnnotationKey]
	var parsed map[string]int64
	require.NoError(t, json.Unmarshal([]byte(annotation), &parsed))
	assert.Equal(t, map[string]int64{dir: 42}, parsed)
}
