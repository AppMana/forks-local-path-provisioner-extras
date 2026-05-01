package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
	flagDebug              = flag.Bool("debug", false, "enable debug logging")
	flagKubeClientBurst    = flag.Int("kube-client-burst", rest.DefaultBurst, "kube client burst")
	flagKubeClientQPS      = flag.Float64("kube-client-qps", float64(rest.DefaultQPS), "kube client QPS")
)

const (
	defaultNamespace          = "local-path-storage"
	defaultHelperImage        = "ghcr.io/appmana/local-path-helper:latest"
	defaultServiceAccountName = "local-path-provisioner-service-account"
	defaultConfigFileKey      = "config.json"
	defaultHelperPodFileKey   = "helperPod.yaml"
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
	srv := csi.NewServer(*flagEndpoint, csi.NewIdentityServer(), nil, csi.NewNodeServer(nodeID))
	logrus.Infof("local-path CSI driver node mode (node=%s driver=%s)", nodeID, csi.DriverName)
	return srv.Serve(ctx)
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
