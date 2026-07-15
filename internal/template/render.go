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

// ErrDirNotEmpty 目标目录非空且未 --force。
var ErrDirNotEmpty = fmt.Errorf("目标目录非空（用 --force 覆盖）")

// Copy 把 tar 中模板 name 逐字解压到 dir（文件直接落在 dir/ 下，零替换）。
// 第二个参数保留 *Registry 以兼容调用方签名，内部直接读全局 templatesTar。
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
			return fmt.Errorf("template: 读 tar: %w", err)
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
