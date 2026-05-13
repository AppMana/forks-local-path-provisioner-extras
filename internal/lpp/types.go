package lpp

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// OS labels used in node.metadata.labels[kubernetes.io/os] and as keys into
// the per-OS helper-pod template / command maps.
const (
	OSLinux   = "linux"
	OSWindows = "windows"
)

const (
	DefaultNodeAffinityKey    = "kubernetes.io/hostname"
	NodeDefaultNonListedNodes = "DEFAULT_PATH_FOR_NON_LISTED_NODES"

	envVolDir       = "VOL_DIR"
	envVolMode      = "VOL_MODE"
	envVolSize      = "VOL_SIZE_BYTES"
	envVolQuotaType = "VOL_QUOTA_TYPE"
	envSnapDir      = "SNAP_DIR"

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
	SnapshotCommand   string `json:"snapshotCommand,omitempty"`
	RestoreCommand    string `json:"restoreCommand,omitempty"`
	// Windows holds per-OS overrides used when the helper pod runs on a
	// node labelled kubernetes.io/os=windows. Each command may be a single
	// string (the whole argv as a shell line, split on whitespace) or an
	// array. When nil or empty, the controller falls back to the built-in
	// Windows defaults baked into the helper image.
	Windows *WindowsConfigData `json:"windows,omitempty"`
	StorageClassConfigData
	StorageClassConfigs map[string]StorageClassConfigData `json:"storageClassConfigs"`
}

// WindowsConfigData mirrors the top-level command paths for Windows nodes.
// Empty values inherit the controller's per-OS defaults.
type WindowsConfigData struct {
	SetupCommand    StringOrArray `json:"setupCommand,omitempty"`
	TeardownCommand StringOrArray `json:"teardownCommand,omitempty"`
	ResizeCommand   StringOrArray `json:"resizeCommand,omitempty"`
	SnapshotCommand StringOrArray `json:"snapshotCommand,omitempty"`
	RestoreCommand  StringOrArray `json:"restoreCommand,omitempty"`
}

// StringOrArray decodes either "powershell -File foo.ps1" (split on spaces)
// or ["powershell", "-File", "foo.ps1"] (already split). The array form is
// the preferred shape because it doesn't tokenize paths containing spaces.
type StringOrArray []string

func (s *StringOrArray) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if len(trimmed) == 0 || trimmed == "null" {
		*s = nil
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	*s = strings.Fields(str)
	return nil
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
	SnapshotCommand   string
	RestoreCommand    string
	Windows           *WindowsConfig
	StorageClassConfig
	StorageClassConfigs map[string]StorageClassConfig
}

// WindowsConfig is the canonicalized per-OS command overrides. Empty
// fields fall back to the per-OS defaults baked into the controller.
type WindowsConfig struct {
	SetupCommand    []string
	TeardownCommand []string
	ResizeCommand   []string
	SnapshotCommand []string
	RestoreCommand  []string
}
