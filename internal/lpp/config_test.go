package lpp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfigJSON = `{
  "nodePathMap": [
    {"node": "n1", "paths": ["/data/a", "/data/b"]},
    {"node": "n2", "paths": [{"path": "/data/c", "maxCapacity": "10Gi"}]},
    {"node": "DEFAULT_PATH_FOR_NON_LISTED_NODES", "paths": ["/opt/lpp"]}
  ],
  "minSize": "1Mi",
  "maxSize": "100Gi"
}`

func TestLoadConfigFile_FromInlineJSON(t *testing.T) {
	cfg, err := LoadConfigFile(validConfigJSON)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Len(t, cfg.NodePathMap, 3)
	assert.Equal(t, "1Mi", cfg.MinSize)
	assert.Equal(t, "100Gi", cfg.MaxSize)
}

func TestCanonicalizeConfig_Default(t *testing.T) {
	raw, err := LoadConfigFile(validConfigJSON)
	require.NoError(t, err)
	cfg, err := CanonicalizeConfig(raw)
	require.NoError(t, err)
	require.Empty(t, cfg.StorageClassConfigs)
	require.Len(t, cfg.NodePathMap, 3)
	require.NotNil(t, cfg.MinSize)
	assert.Equal(t, "1Mi", cfg.MinSize.String())
	require.NotNil(t, cfg.MaxSize)

	// n1: two plain paths, no maxCapacity
	require.Contains(t, cfg.NodePathMap, "n1")
	assert.Nil(t, cfg.NodePathMap["n1"].Paths["/data/a"].MaxCapacity)
	assert.Nil(t, cfg.NodePathMap["n1"].Paths["/data/b"].MaxCapacity)

	// n2: object path with maxCapacity
	require.Contains(t, cfg.NodePathMap, "n2")
	require.NotNil(t, cfg.NodePathMap["n2"].Paths["/data/c"].MaxCapacity)
	assert.Equal(t, "10Gi", cfg.NodePathMap["n2"].Paths["/data/c"].MaxCapacity.String())
}

func TestCanonicalizeConfig_MultiSC(t *testing.T) {
	raw := `{
      "storageClassConfigs": {
        "fast": {"nodePathMap": [{"node":"n1","paths":["/fast"]}]},
        "slow": {"nodePathMap": [{"node":"n1","paths":["/slow"]}]}
      }
    }`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	cfg, err := CanonicalizeConfig(data)
	require.NoError(t, err)
	assert.Len(t, cfg.StorageClassConfigs, 2)
	assert.Contains(t, cfg.StorageClassConfigs, "fast")
	assert.Contains(t, cfg.StorageClassConfigs, "slow")
}

func TestCanonicalizeConfig_RejectsRootPath(t *testing.T) {
	raw := `{"nodePathMap":[{"node":"n1","paths":["/"]}]}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	_, err = CanonicalizeConfig(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "root")
}

func TestCanonicalizeConfig_RejectsRelativePath(t *testing.T) {
	raw := `{"nodePathMap":[{"node":"n1","paths":["data"]}]}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	_, err = CanonicalizeConfig(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must start with /")
}

func TestCanonicalizeConfig_RejectsDuplicateNode(t *testing.T) {
	raw := `{"nodePathMap":[
      {"node":"n1","paths":["/a"]},
      {"node":"n1","paths":["/b"]}
    ]}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	_, err = CanonicalizeConfig(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate node")
}

func TestCanonicalizeConfig_RejectsDuplicatePath(t *testing.T) {
	raw := `{"nodePathMap":[{"node":"n1","paths":["/a","/a"]}]}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	_, err = CanonicalizeConfig(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate path")
}

func TestCanonicalizeConfig_MinExceedsMax(t *testing.T) {
	raw := `{"nodePathMap":[{"node":"n","paths":["/a"]}],"minSize":"10Gi","maxSize":"1Gi"}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	_, err = CanonicalizeConfig(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "minSize")
}

func TestCanonicalizeConfig_BadMaxCapacity(t *testing.T) {
	raw := `{"nodePathMap":[{"node":"n","paths":[{"path":"/a","maxCapacity":"notaquantity"}]}]}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	_, err = CanonicalizeConfig(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "maxCapacity")
}

func TestCanonicalizeConfig_TimeoutDefaults(t *testing.T) {
	raw := `{"nodePathMap":[{"node":"n","paths":["/a"]}]}`
	data, err := LoadConfigFile(raw)
	require.NoError(t, err)
	cfg, err := CanonicalizeConfig(data)
	require.NoError(t, err)
	assert.Equal(t, defaultCmdTimeoutSeconds, cfg.CmdTimeoutSeconds)

	raw = `{"cmdTimeoutSeconds":42,"nodePathMap":[{"node":"n","paths":["/a"]}]}`
	data, err = LoadConfigFile(raw)
	require.NoError(t, err)
	cfg, err = CanonicalizeConfig(data)
	require.NoError(t, err)
	assert.Equal(t, 42, cfg.CmdTimeoutSeconds)
}

func TestNodePathMapData_ParsePaths_Mixed(t *testing.T) {
	raw := `{"node":"n","paths":["/a",{"path":"/b","maxCapacity":"5Gi"}]}`
	var d NodePathMapData
	require.NoError(t, mustUnmarshal(raw, &d))
	entries, err := d.ParsePaths()
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "/a", entries[0].Path)
	assert.Empty(t, entries[0].MaxCapacity)
	assert.Equal(t, "/b", entries[1].Path)
	assert.Equal(t, "5Gi", entries[1].MaxCapacity)
}
