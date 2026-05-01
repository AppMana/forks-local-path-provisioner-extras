package lpp

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	DefaultNodeAffinityKey    = "kubernetes.io/hostname"
	NodeDefaultNonListedNodes = "DEFAULT_PATH_FOR_NON_LISTED_NODES"

	envVolDir       = "VOL_DIR"
	envVolMode      = "VOL_MODE"
	envVolSize      = "VOL_SIZE_BYTES"
	envVolQuotaType = "VOL_QUOTA_TYPE"

	helperScriptDir     = "/script"
	helperDataVolName   = "data"
	helperScriptVolName = "script"

	defaultCmdTimeoutSeconds = 120

	// PV annotations carried over from PR #558 for backward compatibility.
	NodeNameAnnotationKey  = "local.path.provisioner/selected-node"
	QuotaTypeAnnotationKey = "local.path.provisioner/quota-type"
)

// PathEntry represents a single path configuration. It supports both plain
// string paths (legacy) and object paths with maxCapacity.
type PathEntry struct {
	Path        string `json:"path"`
	MaxCapacity string `json:"maxCapacity,omitempty"`
}

// NodePathMapData is the JSON representation of node-to-path mappings.
// Paths can be plain strings or objects with path and maxCapacity fields.
type NodePathMapData struct {
	Node  string            `json:"node,omitempty"`
	Paths []json.RawMessage `json:"paths,omitempty"`
}

// ParsePaths parses the raw JSON path entries into PathEntry objects.
func (n *NodePathMapData) ParsePaths() ([]PathEntry, error) {
	entries := make([]PathEntry, 0, len(n.Paths))
	for _, raw := range n.Paths {
		trimmed := strings.TrimSpace(string(raw))
		if len(trimmed) > 0 && trimmed[0] == '"' {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("failed to parse path string: %v", err)
			}
			entries = append(entries, PathEntry{Path: s})
		} else {
			var entry PathEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return nil, fmt.Errorf("failed to parse path object: %v", err)
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

type StorageClassConfigData struct {
	NodePathMap          []*NodePathMapData `json:"nodePathMap,omitempty"`
	SharedFileSystemPath string             `json:"sharedFileSystemPath,omitempty"`
	MinSize              string             `json:"minSize,omitempty"`
	MaxSize              string             `json:"maxSize,omitempty"`
}

type ConfigData struct {
	CmdTimeoutSeconds int    `json:"cmdTimeoutSeconds,omitempty"`
	SetupCommand      string `json:"setupCommand,omitempty"`
	TeardownCommand   string `json:"teardownCommand,omitempty"`
	ResizeCommand     string `json:"resizeCommand,omitempty"`
	StorageClassConfigData
	StorageClassConfigs map[string]StorageClassConfigData `json:"storageClassConfigs"`
}

type PathConfig struct {
	MaxCapacity *resource.Quantity
}

type NodePathMap struct {
	Paths map[string]*PathConfig
}

type StorageClassConfig struct {
	NodePathMap          map[string]*NodePathMap
	SharedFileSystemPath string
	MinSize              *resource.Quantity
	MaxSize              *resource.Quantity
}

type Config struct {
	CmdTimeoutSeconds int
	SetupCommand      string
	TeardownCommand   string
	ResizeCommand     string
	StorageClassConfig
	StorageClassConfigs map[string]StorageClassConfig
}
