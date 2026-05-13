package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rancher/local-path-provisioner/csi"
	"github.com/rancher/local-path-provisioner/internal/lpp"
)

// VERSION is set via -ldflags by scripts/build.
var VERSION = "0.1.0"

var (
	flagMode               = flag.String("mode", "controller", "operating mode: controller | node")
	flagEndpoint           = flag.String("csi-endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
	flagNodeID             = flag.String("node-id", "", "kubelet node name (controller mode ignores this; node mode requires it; defaults to $NODE_ID)")
	flagKubeconfig         = flag.String("kubeconfig", "", "path to kubeconfig (out-of-cluster only)")
	flagNamespace          = flag.String("namespace", "", "namespace to create helper pods in (defaults to $POD_NAMESPACE or 'local-path-storage')")
	flagHelperImage        = flag.String("helper-image", "", "image used by helper pods (defaults to $HELPER_IMAGE or 'ghcr.io/appmana/local-path-helper:latest')")
	flagConfigMapName      = flag.String("configmap-name", "local-path-config", "ConfigMap holding config.json + setup/teardown/resize scripts")
	flagConfigFile         = flag.String("config", "", "path to config.json (out-of-cluster only; in-cluster uses the ConfigMap)")
	flagHelperPodFile      = flag.String("helper-pod-file", "", "path to helperPod.yaml (out-of-cluster only)")
	flagServiceAccountName = flag.String("service-account-name", "", "ServiceAccount used by helper pods (defaults to $SERVICE_ACCOUNT_NAME or 'local-path-provisioner-service-account')")
	flagDebug                = flag.Bool("debug", false, "enable debug logging")
	flagKubeClientBurst      = flag.Int("kube-client-burst", rest.DefaultBurst, "kube client burst")
	flagKubeClientQPS        = flag.Float64("kube-client-qps", float64(rest.DefaultQPS), "kube client QPS")
	flagCapacityPollInterval = flag.Duration("capacity-poll-interval", 30*time.Second, "node-mode: how often to statfs configured paths and update the Node annotation")
)

const (
	defaultNamespace          = "local-path-storage"
	defaultHelperImage        = "ghcr.io/appmana/local-path-helper:latest"
	defaultServiceAccountName = "local-path-provisioner-service-account"
	defaultConfigFileKey      = "config.json"
	defaultHelperPodFileKey        = "helperPod.yaml"
	defaultHelperPodWindowsFileKey = "helperPod-windows.yaml"
)

func main() {
	flag.Parse()
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	logrus.Infof("local-path-csi version=%s", VERSION)
	if *flagDebug || os.Getenv("RANCHER_DEBUG") != "" {
		logrus.SetLevel(logrus.DebugLevel)
	}

	ctx, cancel := context.WithCancel(context.Background())
	registerShutdown(cancel)

	switch *flagMode {
	case "controller":
		if err := runController(ctx); err != nil {
			logrus.Fatalf("controller: %v", err)
		}
	case "node":
		if err := runNode(ctx); err != nil {
			logrus.Fatalf("node: %v", err)
		}
	default:
		logrus.Fatalf("invalid --mode %q (expected controller|node)", *flagMode)
	}
}

func runController(ctx context.Context) error {
	cfg, err := loadKubeConfig(*flagKubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %v", err)
	}
	cfg.Burst = *flagKubeClientBurst
	cfg.QPS = float32(*flagKubeClientQPS)
	kc, err := clientset.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kube client: %v", err)
	}

	ns := envOr(*flagNamespace, "POD_NAMESPACE", defaultNamespace)
	helperImage := envOr(*flagHelperImage, "HELPER_IMAGE", defaultHelperImage)
	saName := envOr(*flagServiceAccountName, "SERVICE_ACCOUNT_NAME", defaultServiceAccountName)

	configFile := *flagConfigFile
	if configFile == "" {
		configFile, err = readConfigMapKey(kc, ns, *flagConfigMapName, defaultConfigFileKey)
		if err != nil {
			return fmt.Errorf("read config from ConfigMap %s/%s: %v", ns, *flagConfigMapName, err)
		}
	}

	var helperPodYaml string
	if *flagHelperPodFile != "" {
		helperPodYaml, err = lpp.LoadFile(*flagHelperPodFile)
		if err != nil {
			return fmt.Errorf("load helper pod file %s: %v", *flagHelperPodFile, err)
		}
	} else {
		helperPodYaml, err = readConfigMapKey(kc, ns, *flagConfigMapName, defaultHelperPodFileKey)
		if err != nil {
			return fmt.Errorf("read helperPod.yaml from ConfigMap %s/%s: %v", ns, *flagConfigMapName, err)
		}
	}

	prov, err := lpp.NewProvisioner(ctx, kc, configFile, ns, helperImage, *flagConfigMapName, saName, helperPodYaml)
	if err != nil {
		return fmt.Errorf("new provisioner: %v", err)
	}
	// Optional Windows helper-pod template. Missing the configmap key is
	// fine — that just means the controller won't dispatch to Windows
	// nodes (RunHelperPod surfaces a clear error if it tries).
	if windowsYaml, err := readConfigMapKey(kc, ns, *flagConfigMapName, defaultHelperPodWindowsFileKey); err == nil {
		if err := prov.SetWindowsHelperPodTemplate(windowsYaml); err != nil {
			logrus.Warnf("invalid %s in ConfigMap %s/%s: %v", defaultHelperPodWindowsFileKey, ns, *flagConfigMapName, err)
		} else {
			logrus.Infof("loaded Windows helper-pod template from %s", defaultHelperPodWindowsFileKey)
		}
	}

	srv := csi.NewServer(*flagEndpoint, csi.NewIdentityServer(), csi.NewControllerServer(prov), nil)
	logrus.Infof("local-path CSI driver controller mode (driver=%s version=%s)", csi.DriverName, csi.DriverVersion)
	return srv.Serve(ctx)
}

func runNode(ctx context.Context) error {
	nodeID := *flagNodeID
	if nodeID == "" {
		nodeID = os.Getenv("NODE_ID")
	}
	if nodeID == "" {
		return fmt.Errorf("--node-id or $NODE_ID must be set in node mode")
	}

	// Capacity reporter is best-effort: if we can't reach the API or read
	// the configmap, the node still serves CSI RPCs but won't publish
	// free-bytes annotations. The controller's GetCapacity falls back to
	// reporting zero / unknown for that node.
	if err := startCapacityReporter(ctx, nodeID); err != nil {
		logrus.Warnf("capacity reporter not started: %v", err)
	}

	srv := csi.NewServer(*flagEndpoint, csi.NewIdentityServer(), nil, csi.NewNodeServer(nodeID))
	logrus.Infof("local-path CSI driver node mode (node=%s driver=%s)", nodeID, csi.DriverName)
	return srv.Serve(ctx)
}

// startCapacityReporter resolves the per-node configured paths from the
// local-path-config ConfigMap and starts a background goroutine that
// updates the Node annotation periodically.
func startCapacityReporter(ctx context.Context, nodeID string) error {
	cfg, err := loadKubeConfig(*flagKubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %v", err)
	}
	cfg.Burst = *flagKubeClientBurst
	cfg.QPS = float32(*flagKubeClientQPS)
	kc, err := clientset.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kube client: %v", err)
	}

	ns := envOr(*flagNamespace, "POD_NAMESPACE", defaultNamespace)
	configFile := *flagConfigFile
	if configFile == "" {
		configFile, err = readConfigMapKey(kc, ns, *flagConfigMapName, defaultConfigFileKey)
		if err != nil {
			return fmt.Errorf("read config from ConfigMap %s/%s: %v", ns, *flagConfigMapName, err)
		}
	}
	configData, err := lpp.LoadConfigFile(configFile)
	if err != nil {
		return fmt.Errorf("parse config: %v", err)
	}
	canon, err := lpp.CanonicalizeConfig(configData)
	if err != nil {
		return fmt.Errorf("canonicalize config: %v", err)
	}

	paths := pathsForNode(canon, nodeID)
	logrus.Infof("capacity reporter: node %s watching %v (interval %s)", nodeID, paths, *flagCapacityPollInterval)
	rep := lpp.NewCapacityReporter(kc, nodeID, paths, *flagCapacityPollInterval)
	go rep.Run(ctx)
	return nil
}

// pathsForNode returns the union of paths configured for nodeID across
// every StorageClassConfig (including the default), plus paths from the
// DEFAULT_PATH_FOR_NON_LISTED_NODES fallback when nodeID has no explicit
// entry.
func pathsForNode(c *lpp.Config, nodeID string) []string {
	seen := map[string]struct{}{}
	collect := func(sc *lpp.StorageClassConfig) {
		add := func(npm *lpp.NodePathMap) {
			if npm == nil {
				return
			}
			for p := range npm.Paths {
				seen[p] = struct{}{}
			}
		}
		if direct, ok := sc.NodePathMap[nodeID]; ok {
			add(direct)
		} else {
			add(sc.NodePathMap[lpp.NodeDefaultNonListedNodes])
		}
	}
	collect(&c.StorageClassConfig)
	for _, sc := range c.StorageClassConfigs {
		sc := sc
		collect(&sc)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

func loadKubeConfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	if envPath := os.Getenv(clientcmd.RecommendedConfigPathEnvVar); envPath != "" {
		for _, f := range filepath.SplitList(envPath) {
			if _, err := os.Stat(f); err == nil {
				return clientcmd.BuildConfigFromFlags("", f)
			}
		}
	}
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	return clientcmd.BuildConfigFromFlags("", filepath.Join(home, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName))
}

func readConfigMapKey(kc clientset.Interface, namespace, name, key string) (string, error) {
	cm, err := kc.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	v, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q missing in ConfigMap %s/%s", key, namespace, name)
	}
	return v, nil
}

func envOr(value, envKey, def string) string {
	if value != "" {
		return value
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func registerShutdown(cancel context.CancelFunc) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		logrus.Infof("received signal %v, shutting down", s)
		cancel()
	}()
}
