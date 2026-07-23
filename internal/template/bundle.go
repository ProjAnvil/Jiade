package template

import (
	"archive/tar"
	"bytes"
	_ "embed"
	"fmt"
	"io"
)

// templates.tar is packaged from the templates/bank by go:generate (see gen.go).
// Single-file embed: Avoid the limitation that go:embed cannot embed nested modules (bank/go.mod).
//
//go:embed templates.tar
var templatesTar []byte

// readTarFile reads the contents of an ordinary file at the specified path from embedded tar.
func readTarFile(name string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(templatesTar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("template: read tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Name == name {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("template: %s not found in tar archive", name)
}
