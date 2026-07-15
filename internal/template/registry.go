package template

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry 已发现的内嵌模板集合（从 templates.tar）。
type Registry struct{}

// New 验证 templates.tar 已打包。
func New() (*Registry, error) {
	if len(templatesTar) == 0 {
		return nil, fmt.Errorf("template: templates.tar 为空（运行 go generate ./internal/template 重新打包）")
	}
	return &Registry{}, nil
}

// Names 返回 tar 内顶层模板名（排序）。
func (r *Registry) Names() ([]string, error) {
	seen := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(templatesTar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) >= 1 && parts[0] != "" {
			seen[parts[0]] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// Manifest 读某模板的 template.yaml。
func (r *Registry) Manifest(name string) (*Manifest, error) {
	data, err := readTarFile(name + "/template.yaml")
	if err != nil {
		return nil, fmt.Errorf("template: 读 %s/template.yaml: %w", name, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("template: 解析 %s manifest: %w", name, err)
	}
	return &m, nil
}
