package lpp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

type ActionType string

const (
	ActionTypeCreate   ActionType = "create"
	ActionTypeDelete   ActionType = "delete"
	ActionTypeResize   ActionType = "resize"
	ActionTypeSnapshot ActionType = "snapshot"
)

const (
	EnvConfigMountPath   = "CONFIG_MOUNT_PATH"
	DefaultHelperPodFile = "helperPod.yaml"
)

var (
	ConfigFileCheckInterval = 30 * time.Second
	HelperPodNameMaxLength  = 128
)

// Provisioner is the in-cluster core that turns CSI requests (and, in earlier
// revisions, sigs.k8s.io/sig-storage-lib events) into helper-pod runs against
// the configured nodePathMap.
type Provisioner struct {
	ctx                context.Context
	kubeClient         clientset.Interface
	namespace          string
	helperImage        string
	serviceAccountName string

	config          *Config
	configData      *ConfigData
	configFile      string
	configMapName   string
	configMutex     *sync.RWMutex
	helperPod       *v1.Pod
	capacityTracker *CapacityTracker

	// runHelperPodFn is the helper-pod dispatch hook. nil means "use the
	// real implementation". Tests inject a fake via SetRunHelperPodFn.
	runHelperPodFn func(context.Context, HelperAction) error
}

// SetRunHelperPodFn replaces the helper-pod dispatch with a test fake.
// Pass nil to restore the real behavior.
func (p *Provisioner) SetRunHelperPodFn(fn func(context.Context, HelperAction) error) {
	p.runHelperPodFn = fn
}

// NewProvisioner builds a Provisioner and runs an initial config refresh.
// The capacity tracker is rebuilt from existing PVs so allocations survive
// controller restarts.
func NewProvisioner(
	ctx context.Context,
	kubeClient clientset.Interface,
	configFile, namespace, helperImage, configMapName, serviceAccountName, helperPodYaml string,
) (*Provisioner, error) {
	p := &Provisioner{
		ctx:                ctx,
		kubeClient:         kubeClient,
		namespace:          namespace,
		helperImage:        helperImage,
		serviceAccountName: serviceAccountName,
		configFile:         configFile,
		configMapName:      configMapName,
		configMutex:        &sync.RWMutex{},
		capacityTracker:    NewCapacityTracker(),
	}
	var err error
	p.helperPod, err = LoadHelperPodFile(helperPodYaml)
	if err != nil {
		return nil, err
	}
	if err := p.refreshConfig(); err != nil {
		return nil, err
	}
	if err := p.initCapacityTracker(); err != nil {
		logrus.Warnf("failed to initialize capacity tracker from existing PVs: %v", err)
	}
	// Only spawn the watch loop when configFile is an actual path; inline
	// JSON (used in tests and as a fallback) has nothing to refresh.
	if isJSONFile(configFile) {
		p.watchAndRefreshConfig()
	}
	return p, nil
}

// KubeClient returns the underlying Kubernetes clientset.
func (p *Provisioner) KubeClient() clientset.Interface { return p.kubeClient }

// Namespace returns the namespace the provisioner runs helper pods into.
func (p *Provisioner) Namespace() string { return p.namespace }

// SetupCommand returns the configured setup command, or the default if unset.
func (p *Provisioner) SetupCommand() []string {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	if p.config != nil && p.config.SetupCommand != "" {
		return []string{p.config.SetupCommand}
	}
	return []string{"/bin/sh", "/script/setup"}
}

// TeardownCommand returns the configured teardown command, or the default.
func (p *Provisioner) TeardownCommand() []string {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	if p.config != nil && p.config.TeardownCommand != "" {
		return []string{p.config.TeardownCommand}
	}
	return []string{"/bin/sh", "/script/teardown"}
}

// ResizeCommand returns the configured resize command, or the default.
func (p *Provisioner) ResizeCommand() []string {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	if p.config != nil && p.config.ResizeCommand != "" {
		return []string{p.config.ResizeCommand}
	}
	return []string{"/bin/sh", "/script/resize"}
}

func (p *Provisioner) refreshConfig() error {
	p.configMutex.Lock()
	defer p.configMutex.Unlock()
	configData, err := LoadConfigFile(p.configFile)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(configData, p.configData) {
		return nil
	}
	cfg, err := CanonicalizeConfig(configData)
	if err != nil {
		return err
	}
	p.configData = configData
	p.config = cfg
	if out, err := json.Marshal(p.configData); err == nil {
		logrus.Debugf("Applied config: %v", string(out))
	}
	return nil
}

func (p *Provisioner) refreshHelperPod() error {
	p.configMutex.Lock()
	defer p.configMutex.Unlock()
	mountPath, ok := os.LookupEnv(EnvConfigMountPath)
	if !ok {
		return nil
	}
	yaml, err := LoadFile(filepath.Join(mountPath, DefaultHelperPodFile))
	if err != nil {
		return err
	}
	p.helperPod, err = LoadHelperPodFile(yaml)
	return err
}

func (p *Provisioner) watchAndRefreshConfig() {
	go func() {
		ticker := time.NewTicker(ConfigFileCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := p.refreshConfig(); err != nil {
					logrus.Errorf("failed to load the new config file: %v", err)
				}
				if err := p.refreshHelperPod(); err != nil {
					logrus.Errorf("failed to load the new helper pod manifest: %v", err)
				}
			case <-p.ctx.Done():
				logrus.Infof("stop watching config file")
				return
			}
		}
	}()
}

// initCapacityTracker scans PVs provisioned by us and rebuilds the in-memory
// tally so capacity budgets survive controller restarts.
func (p *Provisioner) initCapacityTracker() error {
	pvs, err := p.kubeClient.CoreV1().PersistentVolumes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PVs: %v", err)
	}
	for _, pv := range pvs.Items {
		var node, volPath string
		if pv.Spec.PersistentVolumeSource.CSI != nil && pv.Spec.PersistentVolumeSource.CSI.Driver != "" {
			attrs := pv.Spec.PersistentVolumeSource.CSI.VolumeAttributes
			node = attrs["node"]
			volPath = attrs["path"]
		} else if pv.Annotations != nil {
			node = pv.Annotations[NodeNameAnnotationKey]
			if pv.Spec.PersistentVolumeSource.HostPath != nil {
				volPath = pv.Spec.PersistentVolumeSource.HostPath.Path
			} else if pv.Spec.PersistentVolumeSource.Local != nil {
				volPath = pv.Spec.PersistentVolumeSource.Local.Path
			}
		}
		if node == "" || volPath == "" {
			continue
		}
		base := filepath.Dir(volPath)
		storage := pv.Spec.Capacity[v1.ResourceStorage]
		p.capacityTracker.Allocate(node, base, storage.Value())
		logrus.Debugf("capacity tracker: PV %s on %s:%s = %d bytes", pv.Name, node, base, storage.Value())
	}
	return nil
}

// PickConfig selects the StorageClassConfig matching the given storage class.
// Empty name (or the absence of any StorageClassConfigs in config.json) returns
// the top-level default.
func (p *Provisioner) PickConfig(storageClassName string) (*StorageClassConfig, error) {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	if p.config == nil {
		return nil, fmt.Errorf("no valid config available")
	}
	if len(p.config.StorageClassConfigs) == 0 || storageClassName == "" {
		cp := p.config.StorageClassConfig
		return &cp, nil
	}
	cfg, ok := p.config.StorageClassConfigs[storageClassName]
	if !ok {
		return nil, fmt.Errorf("no config for storage class %q", storageClassName)
	}
	return &cfg, nil
}

// IsSharedFilesystem returns true if the given config uses sharedFileSystemPath
// rather than a per-node nodePathMap.
func (p *Provisioner) IsSharedFilesystem(c *StorageClassConfig) (bool, error) {
	if (c.SharedFileSystemPath != "") && (len(c.NodePathMap) != 0) {
		return false, fmt.Errorf("both nodePathMap and sharedFileSystemPath are defined")
	}
	if len(c.NodePathMap) != 0 {
		return false, nil
	}
	if c.SharedFileSystemPath != "" {
		return true, nil
	}
	return false, fmt.Errorf("both nodePathMap and sharedFileSystemPath are unconfigured")
}

// GetPathOnNode picks (and atomically allocates capacity for) a path on the
// given node. If requestedPath is non-empty it is used as-is; otherwise a path
// with sufficient capacity is chosen at random from the node's pool.
func (p *Provisioner) GetPathOnNode(node, requestedPath string, c *StorageClassConfig, requestedBytes int64) (string, error) {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	if p.config == nil {
		return "", fmt.Errorf("no valid config available")
	}
	sharedFS, err := p.IsSharedFilesystem(c)
	if err != nil {
		return "", err
	}
	if sharedFS {
		return c.SharedFileSystemPath, nil
	}
	npMap := c.NodePathMap[node]
	if npMap == nil {
		npMap = c.NodePathMap[NodeDefaultNonListedNodes]
		if npMap == nil {
			return "", fmt.Errorf("config doesn't contain node %v, and no %v available", node, NodeDefaultNonListedNodes)
		}
	}
	if len(npMap.Paths) == 0 {
		return "", fmt.Errorf("no local path available on node %v", node)
	}
	if requestedPath != "" {
		pc, ok := npMap.Paths[requestedPath]
		if !ok {
			return "", fmt.Errorf("config doesn't contain path %v on node %v", requestedPath, node)
		}
		if !p.capacityTracker.TryAllocate(node, requestedPath, requestedBytes, pc.MaxCapacity) {
			return "", fmt.Errorf("path %v on node %v has insufficient capacity (requested %d bytes)", requestedPath, node, requestedBytes)
		}
		return requestedPath, nil
	}
	eligible := make([]string, 0, len(npMap.Paths))
	for path, pc := range npMap.Paths {
		if p.capacityTracker.HasCapacity(node, path, requestedBytes, pc.MaxCapacity) {
			eligible = append(eligible, path)
		}
	}
	rand.Shuffle(len(eligible), func(i, j int) { eligible[i], eligible[j] = eligible[j], eligible[i] })
	for _, path := range eligible {
		pc := npMap.Paths[path]
		if p.capacityTracker.TryAllocate(node, path, requestedBytes, pc.MaxCapacity) {
			return path, nil
		}
	}
	return "", fmt.Errorf("no local path on node %v has sufficient capacity for %d bytes", node, requestedBytes)
}

// ReleaseCapacity frees a previous TryAllocate. Safe to call on an
// already-released or never-allocated entry.
func (p *Provisioner) ReleaseCapacity(node, basePath string, sizeBytes int64) {
	p.capacityTracker.Release(node, basePath, sizeBytes)
}

// CapacityTracker exposes the tracker for read-only inspection.
func (p *Provisioner) CapacityTracker() *CapacityTracker { return p.capacityTracker }

// VolumeOptions are the per-call inputs to RunHelperPod.
type VolumeOptions struct {
	Name        string
	Path        string
	Mode        v1.PersistentVolumeMode
	SizeInBytes int64
	Node        string
	QuotaType   string
}

// HelperAction bundles the args for a single helper-pod invocation.
type HelperAction struct {
	Type   ActionType
	Cmd    []string
	Volume VolumeOptions
	Config *StorageClassConfig
}

// RunHelperPod creates a one-shot helper pod on the target node and waits for
// it to complete. Pod logs are captured into the controller's log stream.
// Tests can inject a fake via SetRunHelperPodFn.
func (p *Provisioner) RunHelperPod(ctx context.Context, a HelperAction) error {
	if p.runHelperPodFn != nil {
		return p.runHelperPodFn(ctx, a)
	}
	return p.runHelperPodReal(ctx, a)
}

func (p *Provisioner) runHelperPodReal(ctx context.Context, a HelperAction) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to %v volume %v", a.Type, a.Volume.Name)
	}()
	o := a.Volume
	sharedFS, err := p.IsSharedFilesystem(a.Config)
	if err != nil {
		return err
	}
	if o.Name == "" || o.Path == "" || (!sharedFS && o.Node == "") {
		return fmt.Errorf("invalid empty name or path or node")
	}
	if !filepath.IsAbs(o.Path) {
		return fmt.Errorf("volume path %s is not absolute", o.Path)
	}
	o.Path = filepath.Clean(o.Path)
	parentDir, volumeDir := filepath.Split(o.Path)
	hostPathType := v1.HostPathDirectoryOrCreate

	helperPod := p.helperPod.DeepCopy()
	lpvVolumes := []v1.Volume{{
		Name: helperDataVolName,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{Path: parentDir, Type: &hostPathType},
		},
	}}

	keyToPathItems := []v1.KeyToPath{}
	if p.config.SetupCommand == "" {
		keyToPathItems = append(keyToPathItems, v1.KeyToPath{Key: "setup", Path: "setup"})
	}
	if p.config.TeardownCommand == "" {
		keyToPathItems = append(keyToPathItems, v1.KeyToPath{Key: "teardown", Path: "teardown"})
	}
	if p.config.ResizeCommand == "" {
		keyToPathItems = append(keyToPathItems, v1.KeyToPath{Key: "resize", Path: "resize"})
	}
	if len(keyToPathItems) > 0 {
		lpvVolumes = append(lpvVolumes, v1.Volume{
			Name: helperScriptVolName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{Name: p.configMapName},
					Items:                keyToPathItems,
				},
			},
		})
		scriptMount := addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperScriptVolName, helperScriptDir)
		scriptMount.MountPath = helperScriptDir
	}
	dataMount := addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperDataVolName, parentDir)
	parentDir = strings.TrimSuffix(dataMount.MountPath, string(filepath.Separator))
	volumeDir = strings.TrimSuffix(volumeDir, string(filepath.Separator))
	if parentDir == "" || volumeDir == "" || !filepath.IsAbs(parentDir) {
		return fmt.Errorf("invalid path %v for %v: cannot find parent dir or volume dir", a.Type, o.Path)
	}

	env := []v1.EnvVar{
		{Name: envVolDir, Value: filepath.Join(parentDir, volumeDir)},
		{Name: envVolMode, Value: string(o.Mode)},
		{Name: envVolSize, Value: strconv.FormatInt(o.SizeInBytes, 10)},
		{Name: envVolQuotaType, Value: o.QuotaType},
	}

	helperPod.Name = helperPod.Name + "-" + string(a.Type) + "-" + o.Name
	if len(helperPod.Name) > HelperPodNameMaxLength {
		helperPod.Name = helperPod.Name[:HelperPodNameMaxLength]
	}
	helperPod.Namespace = p.namespace
	if helperPod.Spec.NodeName == "" && o.Node != "" {
		helperPod.Spec.NodeName = o.Node
	}
	helperPod.Spec.ServiceAccountName = p.serviceAccountName
	helperPod.Spec.RestartPolicy = v1.RestartPolicyNever
	if helperPod.Spec.Tolerations == nil {
		helperPod.Spec.Tolerations = append(helperPod.Spec.Tolerations, v1.Toleration{
			Key: v1.TaintNodeDiskPressure, Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule,
		})
	}
	helperPod.Spec.Volumes = append(helperPod.Spec.Volumes, lpvVolumes...)
	helperPod.Spec.Containers[0].Command = a.Cmd
	helperPod.Spec.Containers[0].Env = append(helperPod.Spec.Containers[0].Env, env...)
	helperPod.Spec.Containers[0].Args = []string{
		"-p", filepath.Join(parentDir, volumeDir),
		"-s", strconv.FormatInt(o.SizeInBytes, 10),
		"-m", string(o.Mode),
		"-a", string(a.Type),
	}

	logrus.Infof("create the helper pod %s into %s", helperPod.Name, p.namespace)
	pod, err := p.kubeClient.CoreV1().Pods(p.namespace).Create(ctx, helperPod, metav1.CreateOptions{})
	if err != nil && !k8serror.IsAlreadyExists(err) {
		return err
	}
	defer func() {
		if logErr := p.saveHelperPodLogs(pod); logErr != nil {
			logrus.Error(logErr.Error())
		}
		if delErr := p.kubeClient.CoreV1().Pods(p.namespace).Delete(context.TODO(), helperPod.Name, metav1.DeleteOptions{}); delErr != nil {
			logrus.Errorf("unable to delete helper pod: %v", delErr)
		}
	}()

	for i := 0; i < p.config.CmdTimeoutSeconds; i++ {
		got, err := p.kubeClient.CoreV1().Pods(p.namespace).Get(ctx, helperPod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch got.Status.Phase {
		case v1.PodSucceeded:
			pod = got
			logrus.Infof("Volume %v %v on %v:%v", o.Name, a.Type, o.Node, o.Path)
			return nil
		case v1.PodFailed:
			pod = got
			reason := extractPodFailureReason(got)
			logs := p.getHelperPodLogsString(got)
			return fmt.Errorf("helper pod failed: %s (logs: %s)", reason, truncateLogs(logs, 500))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("helper pod timeout after %d seconds", p.config.CmdTimeoutSeconds)
}

func addVolumeMount(mounts *[]v1.VolumeMount, name, mountPath string) *v1.VolumeMount {
	for i, m := range *mounts {
		if m.Name == name {
			if m.MountPath == "" {
				(*mounts)[i].MountPath = mountPath
			}
			return &(*mounts)[i]
		}
	}
	*mounts = append(*mounts, v1.VolumeMount{Name: name, MountPath: mountPath})
	return &(*mounts)[len(*mounts)-1]
}

func (p *Provisioner) saveHelperPodLogs(pod *v1.Pod) (err error) {
	if pod == nil {
		return nil
	}
	defer func() { err = errors.Wrapf(err, "failed to save %s logs", pod.Name) }()
	req := p.kubeClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &v1.PodLogOptions{Container: "helper-pod"})
	stream, err := req.Stream(context.TODO())
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, stream); err != nil {
		return err
	}
	logrus.Infof("Start of %s logs", pod.Name)
	if buf.Len() > 0 {
		for _, line := range strings.Split(strings.Trim(buf.String(), "\n"), "\n") {
			logrus.Info(line)
		}
	}
	logrus.Infof("End of %s logs", pod.Name)
	return nil
}

func (p *Provisioner) getHelperPodLogsString(pod *v1.Pod) string {
	if pod == nil {
		return ""
	}
	req := p.kubeClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &v1.PodLogOptions{Container: "helper-pod"})
	stream, err := req.Stream(context.TODO())
	if err != nil {
		return ""
	}
	defer func() { _ = stream.Close() }()
	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, stream); err != nil {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

func extractPodFailureReason(pod *v1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			t := cs.State.Terminated
			if t.Message != "" {
				return fmt.Sprintf("exit code %d: %s", t.ExitCode, t.Message)
			}
			if t.Reason != "" {
				return fmt.Sprintf("exit code %d (%s)", t.ExitCode, t.Reason)
			}
			return fmt.Sprintf("exit code %d", t.ExitCode)
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return "unknown failure reason"
}

func truncateLogs(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
