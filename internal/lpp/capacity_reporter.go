package lpp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
)

// FreeBytesAnnotationKey is set on each Node by the node-side reporter
// loop. Value is JSON: {"<path>": <freeBytes>, ...}
const FreeBytesAnnotationKey = "local.path.provisioner/free-bytes-by-path"

// StatfsFunc returns the available bytes on the filesystem holding path.
// Defaults to a real statvfs; tests inject a fake.
type StatfsFunc func(path string) (availableBytes int64, err error)

// CapacityReporter is the node-side loop that periodically statfs's the
// configured paths and writes them to a Node annotation. The controller's
// GetCapacity RPC reads the annotation back.
type CapacityReporter struct {
	kubeClient   clientset.Interface
	nodeName     string
	paths        []string
	pollInterval time.Duration
	statfs       StatfsFunc
}

// NewCapacityReporter constructs a reporter. paths must be the absolute
// filesystem paths configured for this node in the local-path-config
// nodePathMap.
func NewCapacityReporter(kc clientset.Interface, nodeName string, paths []string, pollInterval time.Duration) *CapacityReporter {
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	cp := append([]string(nil), paths...)
	sort.Strings(cp)
	return &CapacityReporter{
		kubeClient:   kc,
		nodeName:     nodeName,
		paths:        cp,
		pollInterval: pollInterval,
		statfs:       defaultStatfs,
	}
}

// SetStatfs replaces the statfs implementation. Tests use this to feed
// deterministic free-bytes values.
func (r *CapacityReporter) SetStatfs(f StatfsFunc) { r.statfs = f }

// Run blocks until ctx is done. The first report fires immediately.
func (r *CapacityReporter) Run(ctx context.Context) {
	if r.nodeName == "" {
		logrus.Warn("capacity reporter: NODE_ID empty; not reporting")
		return
	}
	if len(r.paths) == 0 {
		logrus.Infof("capacity reporter: node %s has no configured paths; reporting empty", r.nodeName)
	}
	if err := r.reportOnce(ctx); err != nil {
		logrus.Warnf("capacity reporter: initial report: %v", err)
	}
	t := time.NewTicker(r.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.reportOnce(ctx); err != nil {
				logrus.Warnf("capacity reporter: %v", err)
			}
		}
	}
}

// reportOnce statfs's each configured path and patches the Node annotation.
// When the configured path does not exist yet, we walk up to the closest
// existing ancestor and statfs that — the helper pod creates the volume
// directory inside the configured nodePath on first provision, so the
// scheduler-relevant free space is whichever filesystem the path will
// land on. Paths whose entire chain up to "/" is missing (impossible
// outside tests) and statvfs failures both report 0.
func (r *CapacityReporter) reportOnce(ctx context.Context) error {
	free := make(map[string]int64, len(r.paths))
	for _, p := range r.paths {
		target := p
		for target != "" && target != "/" {
			if _, err := os.Stat(target); err == nil {
				break
			}
			parent := filepath.Dir(target)
			if parent == target {
				target = ""
				break
			}
			target = parent
		}
		if target == "" {
			free[p] = 0
			continue
		}
		bytes, err := r.statfs(target)
		if err != nil {
			free[p] = 0
			continue
		}
		free[p] = bytes
	}
	body, err := json.Marshal(free)
	if err != nil {
		return fmt.Errorf("marshal: %v", err)
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, FreeBytesAnnotationKey, string(body)))
	_, err = r.kubeClient.CoreV1().Nodes().Patch(ctx, r.nodeName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch node %s: %v", r.nodeName, err)
	}
	logrus.Debugf("capacity reporter: node %s -> %s", r.nodeName, body)
	return nil
}

// FreeBytesByPath parses the JSON value the reporter writes into the
// FreeBytesAnnotationKey. Returns nil when the annotation is empty or
// invalid (treat as no info, not zero capacity).
func FreeBytesByPath(annotation string) map[string]int64 {
	if annotation == "" {
		return nil
	}
	out := map[string]int64{}
	if err := json.Unmarshal([]byte(annotation), &out); err != nil {
		return nil
	}
	return out
}
