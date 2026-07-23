package template

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrDirNotEmpty The target directory is not empty and is not --forced.
var ErrDirNotEmpty = fmt.Errorf("target directory is not empty (use --force to overwrite it)")

// Copy extracts the template name in tar to dir verbatim (the file falls directly under dir/, zero replacement).
// The second parameter retains *Registry to be compatible with the caller signature, and the global templatesTar is read directly internally.
func Copy(name string, _ *Registry, dir string, force bool) error {
	if err := checkTarget(dir, force); err != nil {
		return err
	}
	prefix := name + "/"
	tr := tar.NewReader(bytes.NewReader(templatesTar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("template: read tar: %w", err)
		}
		if !strings.HasPrefix(hdr.Name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(hdr.Name, prefix)
		if rel == "" {
			continue
		}
		dst := filepath.Join(dir, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			out, err := os.Create(dst)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}

func checkTarget(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return ErrDirNotEmpty
	}
	return os.MkdirAll(dir, 0o755)
}
