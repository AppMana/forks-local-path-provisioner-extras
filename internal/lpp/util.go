package lpp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func LoadFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func LoadHelperPodFile(helperPodYaml string) (*v1.Pod, error) {
	helperPodJSON, err := yaml.YAMLToJSON([]byte(helperPodYaml))
	if err != nil {
		return nil, fmt.Errorf("invalid YAMLToJSON the helper pod with helperPodYaml: %v", helperPodYaml)
	}
	p := v1.Pod{}
	if err := json.Unmarshal(helperPodJSON, &p); err != nil {
		return nil, fmt.Errorf("invalid unmarshal the helper pod with helperPodJSON: %v", string(helperPodJSON))
	}
	if len(p.Spec.Containers) == 0 {
		return nil, fmt.Errorf("helper pod template does not specify any container")
	}
	return &p, nil
}

// pvMetadata is the substitution context for pathPattern templates.
type pvMetadata struct {
	PVName string
	PVC    pvcMetadata
}

type pvcMetadata struct {
	Namespace string
	Name      string
}

// RenderPathPattern substitutes {{.PVName}} / {{.PVC.Namespace}} / {{.PVC.Name}}
// in pattern. When allowUnsafePath is false, the result must start with
// "<namespace>/<pvc-name>/".
func RenderPathPattern(pattern, pvName, pvcNamespace, pvcName string, allowUnsafePath bool) (string, error) {
	tpl, err := template.New("pathPattern").Parse(pattern)
	if err != nil {
		return "", err
	}
	buf := new(bytes.Buffer)
	if err := tpl.Execute(buf, pvMetadata{
		PVName: pvName,
		PVC:    pvcMetadata{Namespace: pvcNamespace, Name: pvcName},
	}); err != nil {
		return "", err
	}
	if allowUnsafePath {
		return buf.String(), nil
	}
	path := buf.String()
	prefix := filepath.Join(pvcNamespace, pvcName) + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("pathPattern must start with {{ .PVC.Namespace }}/{{ .PVC.Name }}/: %s", path)
	}
	return path, nil
}
