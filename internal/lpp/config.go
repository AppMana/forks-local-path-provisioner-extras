package lpp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

func isJSONFile(p string) bool { return strings.HasSuffix(p, ".json") }

func LoadConfigFile(configFile string) (cfg *ConfigData, err error) {
	defer func() { err = errors.Wrapf(err, "fail to load config file %v", configFile) }()
	if !isJSONFile(configFile) {
		var data ConfigData
		if err := json.Unmarshal([]byte(configFile), &data); err != nil {
			return nil, err
		}
		return &data, nil
	}
	f, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var data ConfigData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, err
	}
	return &data, nil
}

func CanonicalizeConfig(data *ConfigData) (*Config, error) {
	cfg := &Config{}
	if len(data.StorageClassConfigs) == 0 {
		def, err := canonicalizeStorageClassConfig(&data.StorageClassConfigData)
		if err != nil {
			return nil, err
		}
		cfg.StorageClassConfig = *def
	} else {
		cfg.StorageClassConfigs = make(map[string]StorageClassConfig, len(data.StorageClassConfigs))
		for name, classData := range data.StorageClassConfigs {
			cc, err := canonicalizeStorageClassConfig(&classData)
			if err != nil {
				return nil, errors.Wrapf(err, "config for class %s is invalid", name)
			}
			cfg.StorageClassConfigs[name] = *cc
		}
	}
	cfg.SetupCommand = data.SetupCommand
	cfg.TeardownCommand = data.TeardownCommand
	cfg.ResizeCommand = data.ResizeCommand
	cfg.SnapshotCommand = data.SnapshotCommand
	cfg.RestoreCommand = data.RestoreCommand
	if data.CmdTimeoutSeconds > 0 {
		cfg.CmdTimeoutSeconds = data.CmdTimeoutSeconds
	} else {
		cfg.CmdTimeoutSeconds = defaultCmdTimeoutSeconds
	}
	return cfg, nil
}

func canonicalizeStorageClassConfig(data *StorageClassConfigData) (cfg *StorageClassConfig, err error) {
	defer func() { err = errors.Wrapf(err, "StorageClass config canonicalization failed") }()
	cfg = &StorageClassConfig{SharedFileSystemPath: data.SharedFileSystemPath}

	if data.MinSize != "" {
		q, err := resource.ParseQuantity(data.MinSize)
		if err != nil {
			return nil, fmt.Errorf("invalid minSize %q: %v", data.MinSize, err)
		}
		cfg.MinSize = &q
	}
	if data.MaxSize != "" {
		q, err := resource.ParseQuantity(data.MaxSize)
		if err != nil {
			return nil, fmt.Errorf("invalid maxSize %q: %v", data.MaxSize, err)
		}
		cfg.MaxSize = &q
	}
	if cfg.MinSize != nil && cfg.MaxSize != nil && cfg.MinSize.Cmp(*cfg.MaxSize) > 0 {
		return nil, fmt.Errorf("minSize %v must not exceed maxSize %v", cfg.MinSize, cfg.MaxSize)
	}

	cfg.NodePathMap = map[string]*NodePathMap{}
	for _, n := range data.NodePathMap {
		if cfg.NodePathMap[n.Node] != nil {
			return nil, fmt.Errorf("duplicate node %v", n.Node)
		}
		npMap := &NodePathMap{Paths: map[string]*PathConfig{}}
		cfg.NodePathMap[n.Node] = npMap

		entries, err := n.ParsePaths()
		if err != nil {
			return nil, fmt.Errorf("failed to parse paths for node %v: %v", n.Node, err)
		}
		for _, e := range entries {
			if len(e.Path) == 0 || e.Path[0] != '/' {
				return nil, fmt.Errorf("path must start with / for path %v on node %v", e.Path, n.Node)
			}
			abs, err := filepath.Abs(e.Path)
			if err != nil {
				return nil, err
			}
			if abs == "/" {
				return nil, fmt.Errorf("cannot use root ('/') as path on node %v", n.Node)
			}
			if _, ok := npMap.Paths[abs]; ok {
				return nil, fmt.Errorf("duplicate path %v on node %v", e.Path, n.Node)
			}
			pc := &PathConfig{}
			if e.MaxCapacity != "" {
				q, err := resource.ParseQuantity(e.MaxCapacity)
				if err != nil {
					return nil, fmt.Errorf("invalid maxCapacity %q for path %v on node %v: %v", e.MaxCapacity, e.Path, n.Node, err)
				}
				pc.MaxCapacity = &q
			}
			npMap.Paths[abs] = pc
		}
	}
	return cfg, nil
}
