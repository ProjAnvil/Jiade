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

// Registry A collection of discovered inline templates (from templates.tar).
type Registry struct{}

// New Verify templates.tar is packaged.
func New() (*Registry, error) {
	if len(templatesTar) == 0 {
		return nil, fmt.Errorf("template: templates.tar is empty (run go generate ./internal/template to rebuild it)")
	}
	return &Registry{}, nil
}

// Names returns the top-level template name in tar (sorted).
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

// Manifest reads template.yaml of a certain template.
func (r *Registry) Manifest(name string) (*Manifest, error) {
	data, err := readTarFile(name + "/template.yaml")
	if err != nil {
		return nil, fmt.Errorf("template: read %s/template.yaml: %w", name, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("template: parse %s manifest: %w", name, err)
	}
	return &m, nil
}
